package aichat

// 本文件实现签核意图（approval）的草案生成流程，与搜索草稿流程
// （search.go / search_agent.go）同构：approval 智能体经 AgentQA 执行，
// 最终答案不向客户端流式输出，而是解析为「正文说明 + 草案」JSON，
// 结构卡控后合并为一条 approval_draft 事件（其后不追加 answer_done）。
// draftId/revision/status/任务绑定由后端代码组装与校验，模型只产出
// 正文与 form，不信任模型输出的任何标识。生成规则（协议文档 3）由
// approval 智能体的系统提示词承载，参考全文见 APPROVAL_PROMPT.md。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// approvalModelOutput 是 approval 智能体的输出契约（approval_draft 事件的
// 源头）：message 是给用户的正文说明（目标/建议/需补充/下一步四部分）；
// drafts 为空表示本轮纯正文回答（普通咨询或澄清），不生成草案。
// draftId/revision/status 由后端组装，模型不产出。
type approvalModelOutput struct {
	Message string               `json:"message"`
	Drafts  []approvalModelDraft `json:"drafts"`
}

// approvalModelDraft 是模型产出的一份草案：绑定标识 + 完整 form。
// 绑定必须与当前激活任务一致（validateApprovalDrafts 卡控）。
type approvalModelDraft struct {
	ActivityID   string        `json:"activityId"`
	AssignmentID string        `json:"assignmentId"`
	Form         *ApprovalForm `json:"form"`
}

// parseApprovalOutput 提取并解析模型输出的 JSON envelope（复用搜索流的
// JSON 对象提取，容忍代码块围栏等前后缀）。
func parseApprovalOutput(content string) (*approvalModelOutput, error) {
	jsonStr := extractJSONObject(content)
	var out approvalModelOutput
	if err := json.Unmarshal([]byte(jsonStr), &out); err != nil {
		return nil, fmt.Errorf("decode approval output JSON: %w", err)
	}
	return &out, nil
}

// assembleApprovalDraft 用后端组装的标识（draftId/revision/status/绑定）
// 包裹模型产出的 form。同一草案优化沿用 draftId 且 revision 递增；新签审
// 需求创建新实例（revision 1）。
func assembleApprovalDraft(modelDraft approvalModelDraft, binding ApprovalTaskBinding, inputDraft *ApprovalDraft) *ApprovalDraft {
	d := &ApprovalDraft{
		ActivityID:   binding.ActivityID,
		AssignmentID: binding.AssignmentID,
		Status:       ApprovalStatusEditing,
		Form:         modelDraft.Form.normalize(),
	}
	if inputDraft != nil && inputDraft.DraftID != "" {
		d.DraftID = inputDraft.DraftID
		d.Revision = inputDraft.Revision + 1
	} else {
		d.DraftID = uuid.NewString()
		d.Revision = 1
	}
	return d
}

// handleApprovalMessage 处理签核意图的消息：同步动态字段、解析五数组、
// 组装结构化上下文（含输入草案状态与保存/完成条件判定），随后以 AgentQA
// 驱动 approval 智能体生成草案。该流程只在「签核开关开启 + 页面为
// processApproval」时被触发（见 intent.go 的 shouldApprove）。
func (h *AIChatHandler) handleApprovalMessage(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	agent *types.CustomAgent,
	tenantID uint64,
) {
	// 主智能体参数仅保持 intentHandler 接口形状；签核轮始终使用专用
	// approval 智能体（内联 → approval_agent_id → 主智能体）。

	approvalAgent, err := h.resolveApprovalAgent(ctx)
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, err.Error(), AIErrorCodeApprovalDraft))
		return
	}

	session, err := h.getOrCreateSession(ctx, req.ConversationID, tenantID)
	if err != nil || session == nil {
		h.writeEvent(c, h.errorEvent(req, "failed to create session", AIErrorCodeApprovalDraft))
		return
	}

	// 动态字段同步（整体替换语义）：携带新值时合并落库，省略时沿用旧值。
	h.syncApprovalContext(ctx, session, req)
	if len(req.ItemData) == 0 || len(req.ApprovalTaskIndex) == 0 {
		if ac := h.loadApprovalContext(ctx, session.ID); ac != nil {
			if len(req.ItemData) == 0 {
				req.ItemData = ac.ItemData
			}
			if len(req.ApprovalTaskIndex) == 0 {
				req.ApprovalTaskIndex = ac.ApprovalTaskIndex
			}
		}
	}
	page := req.Page
	if page == nil {
		if p, _, _, _ := h.loadStartContext(ctx, session.ID); p != nil {
			page = p
			req.Page = p
		}
	}

	// itemData 结构无效属于数据错误，不能当「没有必填项」处理。
	data, err := parseApprovalData(req.ItemData)
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, "invalid itemData payload: "+err.Error(), AIErrorCodeApprovalDraft))
		return
	}

	// 持久化用户消息，保证会话历史完整（签核意图不携带附件，见 handleMessage）。
	if _, err := h.messageService.CreateMessage(ctx, &types.Message{
		SessionID:   session.ID,
		Role:        "user",
		Content:     req.Message,
		RequestID:   req.RequestID,
		IsCompleted: true,
		CreatedAt:   time.Now(),
	}); err != nil {
		h.writeEvent(c, h.errorEvent(req, "failed to persist user message: "+err.Error(), AIErrorCodeApprovalDraft))
		return
	}

	// 绑定与输入事实：激活卡片 -> 匹配上下文 -> 正在编辑的草案（本轮人工
	// 编辑的完整事实）。输入表单的保存/完成条件在此确定性判定后注入元数据。
	var binding *ApprovalTaskBinding
	var cfg *ApprovalConfigEntry
	var inputDraft *ApprovalDraft
	if len(data.MasterItem) > 0 {
		if b, ok := parseMasterItemBinding(data.MasterItem[0]); ok {
			binding = &b
			cfg = data.matchConfig(b)
			inputDraft = data.editingDraft(b)
		}
	}
	inputForm := cfgCurrentOrDraftForm(cfg, inputDraft)
	var readiness *approvalReadiness
	if cfg != nil {
		r := computeApprovalReadiness(inputForm, cfg)
		readiness = &r
	}
	metadata := buildApprovalMetadata(page, req.ApprovalTaskIndex, req.ItemData, binding, inputDraft, readiness)

	// 与搜索草稿流一致：优先使用 approval agent 自己配置的模型，
	// ai_chat.model_id 只在其未声明模型时兜底（经 QARequest.SummaryModelID）。
	qaModelID := ""
	if strings.TrimSpace(approvalAgent.Config.ModelID) == "" &&
		h.config != nil && h.config.AIChat != nil {
		qaModelID = h.config.AIChat.ModelID
	}

	// 预创建助手消息：正文 + 组装后的草案 JSON 落库，后续轮次的优化
	// （「把意见写简洁一些」）能基于历史草案继续。
	assistantMsg, err := h.messageService.CreateMessage(ctx, &types.Message{
		SessionID:     session.ID,
		Role:          "assistant",
		RequestID:     req.RequestID,
		IsCompleted:   false,
		CreatedAt:     time.Now(),
		AgentID:       approvalAgent.ID,
		AgentTenantID: approvalAgent.TenantID,
		ModelID:       approvalAgent.Config.ModelID,
	})
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, "failed to persist assistant message: "+err.Error(), AIErrorCodeApprovalDraft))
		return
	}

	// Query 保持用户原始消息；结构化上下文经 Metadata 注入，由 AgentQA
	// 以 [用户提供的结构化上下文] 追加到智能体查询之后。
	qaReq := &types.QARequest{
		Session:            session,
		Query:              req.Message,
		AssistantMessageID: assistantMsg.ID,
		CustomAgent:        approvalAgent,
		SummaryModelID:     qaModelID,
		WebSearchEnabled:   approvalAgent.Config.WebSearchEnabled,
		Metadata:           metadata,
	}
	logger.Infof(ctx, "[ai-chat] approval draft via AgentQA: agent=%s model=%s binding=%+v hasEditingDraft=%t",
		approvalAgent.ID, approvalAgent.Config.ModelID, binding, inputDraft != nil)

	bus := event.NewEventBus()
	done := make(chan struct{})
	base := newAIStreamTranslator(h, c, req, assistantMsg, session.TenantID, done)
	translator := newApprovalDraftTranslator(base, &approvalDraftDeps{
		h:          h,
		agent:      approvalAgent,
		message:    req.Message,
		metadata:   metadata,
		lang:       detectReplyLanguage(req.Message),
		page:       page,
		itemData:   req.ItemData,
		binding:    binding,
		cfg:        cfg,
		inputDraft: inputDraft,
	})
	translator.subscribe(bus)

	go func() {
		defer translator.close()
		// 装配失败在引擎创建前直接返回，需补发终止错误帧（同搜索草稿流）。
		if err := h.sessionService.AgentQA(ctx, qaReq, bus); err != nil {
			t := translator
			if t.c.Request.Context().Err() == nil {
				t.writeMu.Lock()
				t.h.writeEvent(t.c, t.h.errorEvent(t.req, err.Error(), AIErrorCodeApprovalDraft))
				t.writeMu.Unlock()
			}
		}
	}()

	select {
	case <-done:
	case <-c.Request.Context().Done():
	}
}

// cfgCurrentOrDraftForm 返回输入表单：优先本轮回传的 editing 草案 form
// （用户最新完整编辑），否则用匹配上下文的 currentForm（初次生成初值）。
func cfgCurrentOrDraftForm(cfg *ApprovalConfigEntry, inputDraft *ApprovalDraft) *ApprovalForm {
	if inputDraft != nil && inputDraft.Form != nil {
		return inputDraft.Form
	}
	if cfg != nil {
		return cfg.CurrentForm
	}
	return nil
}

// buildApprovalMetadata 组装签核草案生成的结构化上下文（与 buildSearchMetadata
// 注入方式一致）。代码只注入数据事实：页面、任务索引、itemData 原文（含草案
// 结束标记）、当前激活任务绑定、正在编辑的草案、输入表单的保存/完成条件判定；
// 任务指令与输出契约完全由 approval agent 的系统提示词定义，避免双份契约。
func buildApprovalMetadata(
	page *AIPageContext,
	approvalTaskIndex json.RawMessage,
	itemData json.RawMessage,
	binding *ApprovalTaskBinding,
	inputDraft *ApprovalDraft,
	readiness *approvalReadiness,
) types.JSON {
	payload := struct {
		Page               *AIPageContext       `json:"page,omitempty"`
		ApprovalTaskIndex  json.RawMessage      `json:"approvalTaskIndex,omitempty"`
		ItemData           json.RawMessage      `json:"itemData,omitempty"`
		ActiveTask         *ApprovalTaskBinding `json:"activeTask,omitempty"`
		EditingDraft       *ApprovalDraft       `json:"editingDraft,omitempty"`
		InputFormReadiness *approvalReadiness   `json:"inputFormReadiness,omitempty"`
	}{
		Page:               page,
		ApprovalTaskIndex:  approvalTaskIndex,
		ItemData:           itemData,
		ActiveTask:         binding,
		EditingDraft:       inputDraft,
		InputFormReadiness: readiness,
	}
	b, _ := json.Marshal(payload)
	return types.JSON(b)
}

// approvalDraftDeps 汇聚签核草案完成路径所需的依赖：纠正重试所需的智能体
// 与提示词材料，以及组装/校验草案所需的绑定事实。
type approvalDraftDeps struct {
	h          *AIChatHandler
	agent      *types.CustomAgent // approval 智能体（未应用全局模型覆盖）
	message    string             // 用户原始请求
	metadata   types.JSON         // 主路径注入的结构化上下文（重试复用）
	lang       string
	page       *AIPageContext       // 页面（推荐问题生成上下文）
	itemData   json.RawMessage      // itemData 原文（推荐问题生成上下文）
	binding    *ApprovalTaskBinding // 当前激活任务；nil 表示未激活
	cfg        *ApprovalConfigEntry // 匹配的签审上下文；nil 表示数据未取得
	inputDraft *ApprovalDraft       // 输入中处于 editing 的草案（可无）
}

// repair 在模型输出违反输出契约（非 JSON、缺正文、草案结构违约）时，把
// 原提示词、违约输出与违约说明一并回传模型做一次纠正重试。裸调用没有
// 引擎侧渲染，系统提示词先手动过一遍占位符（current_time 等）。
func (d *approvalDraftDeps) repair(ctx context.Context, badOutput string, problems []string) (*approvalModelOutput, error) {
	chatModel, err := d.h.modelService.GetChatModel(ctx, d.agent.Config.ModelID)
	if err != nil {
		return nil, err
	}
	systemPrompt := types.RenderPromptPlaceholders(d.agent.Config.SystemPrompt, types.PlaceholderValues{
		"language": d.lang,
	})
	var sb strings.Builder
	sb.WriteString("你上一次的输出违反了输出契约：\n")
	for i, p := range problems {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, p))
	}
	sb.WriteString("请严格按系统提示词定义的输出格式重新输出完整 JSON（必须包含 message；" +
		"若当前没有激活任务或不满足生成条件，drafts 返回空数组），只输出一个纯 JSON 对象，禁止解释文字。")
	resp, err := chatModel.Chat(ctx, []chat.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: buildApprovalPrompt(d.message, d.metadata, d.lang)},
		{Role: "assistant", Content: badOutput},
		{Role: "user", Content: sb.String()},
	}, &chat.ChatOptions{Temperature: 0.1})
	if err != nil {
		return nil, err
	}
	return parseApprovalOutput(resp.Content)
}

// buildApprovalPrompt 组装纠正重试所需的用户侧上下文（与主路径 Metadata
// 注入的内容一致），保证重试与主路径看到同一份事实。
func buildApprovalPrompt(message string, metadata types.JSON, lang string) string {
	var sb strings.Builder
	sb.WriteString("[用户提供的结构化上下文]\n")
	sb.WriteString(string(metadata))
	sb.WriteString("\n\n用户请求：" + message)
	sb.WriteString("\n\n[回复语言：" + lang + "]")
	return sb.String()
}

// approvalDraftTranslator 订阅智能体 EventBus：进度事件复用 normal 流的 SSE
// 映射，最终答案仅本地累积，完成时走签核草案的解析/校验/组装/事件输出。
type approvalDraftTranslator struct {
	*aiStreamTranslator
	deps *approvalDraftDeps
}

func newApprovalDraftTranslator(base *aiStreamTranslator, deps *approvalDraftDeps) *approvalDraftTranslator {
	return &approvalDraftTranslator{aiStreamTranslator: base, deps: deps}
}

// subscribe 注册签核草案流程的事件映射：最终答案解析为「正文 + 草案」
// envelope，结构卡控（违约先纠正重试一次），后端组装 draftId/revision/
// status 后落库助手消息，并发出单条 approval_draft 终止事件。
func (t *approvalDraftTranslator) subscribe(bus *event.EventBus) {
	t.subscribeProgress(bus)

	bus.On(event.EventAgentFinalAnswer, func(ctx context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentFinalAnswerData)
		if !ok {
			return nil
		}
		if data.Content != "" {
			t.mu.Lock()
			t.final.WriteString(data.Content)
			t.mu.Unlock()
		}
		return nil
	})

	bus.On(event.EventAgentComplete, func(ctx context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentCompleteData)
		if !ok {
			return nil
		}
		t.mu.Lock()
		final := t.final.String()
		t.mu.Unlock()
		if final == "" {
			final = data.FinalAnswer
		}
		logger.Infof(ctx, "[ai-chat] approval draft model output: %s", final)

		// 契约校验：可解析、正文非空、草案结构合法。违约时回传违约输出
		// 做一次纠正重试，仍违约则按契约违约报错（不发出不可用草案）。
		result, parseErr := parseApprovalOutput(final)
		problems := approvalOutputProblems(result, parseErr, t.deps.binding, t.deps.cfg)
		if len(problems) > 0 {
			repaired, repairErr := t.deps.repair(ctx, final, problems)
			if repairErr != nil {
				logger.Warnf(ctx, "[ai-chat] approval draft repair failed: %v", repairErr)
			} else if repairedProblems := approvalOutputProblems(repaired, nil, t.deps.binding, t.deps.cfg); len(repairedProblems) == 0 {
				result = repaired
				problems = nil
			} else {
				problems = repairedProblems
			}
		}

		// 组装草案：绑定/状态/版本由代码决定；drafts 为空表示纯正文回答。
		var drafts []*ApprovalDraft
		if len(problems) == 0 && t.deps.binding != nil {
			for _, md := range result.Drafts {
				drafts = append(drafts, assembleApprovalDraft(md, *t.deps.binding, t.deps.inputDraft))
			}
		}

		// 落库助手消息只存正文纯文本：envelope JSON 仅在事件里交付。历史若存
		// envelope，后续 normal 意图的主智能体会模仿该格式，把普通回答也输出成
		// JSON 文本（answer_chunk 拼出一段 JSON）。多轮优化不依赖历史里的草案
		// JSON：前端按规格把正在编辑的草案经 itemData.approvalDrafts 回传，
		// 那才是每轮草案取值的权威输入。
		persisted := final
		if len(problems) == 0 {
			persisted = result.Message
		}
		updateCtx := context.WithValue(context.WithoutCancel(ctx), types.TenantIDContextKey, t.tenantID)
		t.asstMsg.Content = persisted
		if steps, ok := data.AgentSteps.([]types.AgentStep); ok {
			t.asstMsg.AgentSteps = agenttools.SanitizeAgentStepsForStorage(steps)
		}
		t.asstMsg.IsCompleted = true
		t.asstMsg.UpdatedAt = time.Now()
		if err := t.h.messageService.UpdateMessage(updateCtx, t.asstMsg); err != nil {
			logger.Warnf(ctx, "failed to persist assistant message %s: %v", t.asstMsg.ID, err)
		}

		if len(problems) > 0 {
			msg := "approval draft output violates the contract: " + strings.Join(problems, "；")
			// 客户端断开后不再写入已结束的响应（write 直连路径需自带与
			// aiStreamTranslator.write 相同的守卫，否则后台完成协程 flush
			// 已回收的 response 会 panic）。
			if t.c.Request.Context().Err() == nil {
				t.writeMu.Lock()
				t.h.writeEvent(t.c, t.h.errorEvent(t.req, msg, AIErrorCodeApprovalDraft))
				t.writeMu.Unlock()
			}
			t.close()
			return nil
		}

		// 与搜索草稿一致：基于会话数据与本轮正文生成本轮推荐问题，随事件返回。
		var suggestions []AISuggestionItem
		if recommendAgent, rerr := t.h.resolveRecommendAgent(ctx); rerr == nil {
			if s, serr := t.h.generateRecommendQuestions(ctx, recommendAgent, t.deps.page, nil, nil, t.deps.itemData, result.Message); serr == nil {
				suggestions = s
			}
		}

		// 单条 approval_draft 终止事件：正文说明复用 message 字段，草案在
		// approvalDrafts 数组，推荐问题在 suggestions（与 search_draft 携带
		// Conditions/Suggestions 的方式一致，不追加 answer_done，也不回显
		// 请求侧数据）。
		if t.c.Request.Context().Err() != nil {
			t.close()
			return nil
		}
		t.writeMu.Lock()
		t.h.writeEvent(t.c, AIEvent{
			Version:        t.req.Version,
			Type:           AIEventApprovalDraft,
			ConversationID: t.req.ConversationID,
			RequestID:      t.req.RequestID,
			Message:        result.Message,
			ApprovalDrafts: drafts,
			Suggestions:    suggestions,
		})
		t.writeMu.Unlock()
		t.close()
		return nil
	})

	bus.On(event.EventError, func(ctx context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.ErrorData)
		if !ok {
			return nil
		}
		t.writeMu.Lock()
		t.h.writeEvent(t.c, t.h.errorEvent(t.req, data.Error, AIErrorCodeApprovalDraft))
		t.writeMu.Unlock()
		t.close()
		return nil
	})
}

// approvalOutputProblems 汇总一次模型输出的全部契约问题：解析失败、正文
// 缺失、草案结构违约（validateApprovalDrafts）。空切片表示可用。
func approvalOutputProblems(result *approvalModelOutput, parseErr error, binding *ApprovalTaskBinding, cfg *ApprovalConfigEntry) []string {
	if parseErr != nil || result == nil {
		return []string{"输出不是合法的 JSON envelope：" + fmt.Sprintf("%v", parseErr)}
	}
	var problems []string
	if strings.TrimSpace(result.Message) == "" {
		problems = append(problems, "缺少必需的 message 字段（给用户的正文说明）")
	}
	problems = append(problems, validateApprovalDrafts(result.Drafts, binding, cfg)...)
	return problems
}

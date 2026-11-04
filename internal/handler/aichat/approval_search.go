package aichat

// 本文件实现 approval_search 意图（签核任务筛选）的处理：在 processApproval
// 页面，用户要求按条件查找/列出签核任务时，由专用 approval_search 智能体在
// 前端提供的 approvalTaskIndex（完整扁平任务索引）中筛选，模型只产出
// activityId + assignmentId 绑定；后端校验绑定后发出单条
// approval_task_selection 终止事件（selectedTasks 只含任务绑定）。
//
// 该流程不签核、不生成签审草案：像「帮我签所有ECO」这类先找后签的批量指令
// 只返回匹配任务，实际签核仍由用户在前端确认后走 approval 流程。筛选规则
// （默认未签审、时间/类型/状态口径、输出契约）由 approval_search 智能体的
// system_prompt 承载，参考全文见 APPROVAL_SEARCH_PROMPT.md。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// AIErrorCodeApprovalTask 是签核任务筛选失败的错误类别标识（error 事件 code）。
const AIErrorCodeApprovalTask = "APPROVAL_TASK_ERROR"

// ApprovalTaskRef 是模型输出的任务引用。模型只产出活动+指派绑定，完整任务
// 条目由后端按绑定从 approvalTaskIndex 组装，避免模型改写任务字段或编造任务。
type ApprovalTaskRef struct {
	ActivityID   string `json:"activityId"`
	AssignmentID string `json:"assignmentId"`
}

// approvalTaskSearchOutput 是 approval_search 智能体的输出契约：message 是
// 给用户的正文说明，tasks 是匹配到的任务绑定（0..N）。
type approvalTaskSearchOutput struct {
	Message string            `json:"message"`
	Tasks   []ApprovalTaskRef `json:"tasks"`
}

// approvalTaskIndexEntry 是任务索引中的一条任务绑定。筛选结果只下发
// activityId + assignmentId，不再需要保留原始索引条目。
type approvalTaskIndexEntry struct {
	activityID   string
	assignmentID string
}

// parseApprovalTaskIndex 把前端提供的扁平任务索引解析为可校验的条目列表。
// 单条缺少 activityId/assignmentId 的脏数据跳过（无法参与筛选与前端添加），
// 非数组或非对象属于协议数据错误，直接返回错误。
func parseApprovalTaskIndex(raw json.RawMessage) ([]approvalTaskIndexEntry, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("approvalTaskIndex is not an array: %w", err)
	}
	entries := make([]approvalTaskIndexEntry, 0, len(items))
	for i, item := range items {
		var ref ApprovalTaskRef
		if err := json.Unmarshal(item, &ref); err != nil {
			return nil, fmt.Errorf("approvalTaskIndex[%d] is not an object: %w", i, err)
		}
		if ref.ActivityID == "" || ref.AssignmentID == "" {
			continue
		}
		entries = append(entries, approvalTaskIndexEntry{
			activityID:   ref.ActivityID,
			assignmentID: ref.AssignmentID,
		})
	}
	return entries, nil
}

// parseApprovalTaskSearchOutput 提取并解析模型输出的 JSON envelope（复用搜索
// 流的 JSON 对象提取，容忍代码块围栏等前后缀）。
func parseApprovalTaskSearchOutput(content string) (*approvalTaskSearchOutput, error) {
	jsonStr := extractJSONObject(content)
	var out approvalTaskSearchOutput
	if err := json.Unmarshal([]byte(jsonStr), &out); err != nil {
		return nil, fmt.Errorf("decode approval task search output JSON: %w", err)
	}
	return &out, nil
}

// selectApprovalTasks 校验并去重模型返回的任务绑定：绑定必须同时匹配
// activityId 与 assignmentId；重复绑定只保留首次出现；索引中不存在的绑定
// 计入 problems，由调用方做一次纠正重试。返回值是可直接下发给前端的
// selectedTasks 绑定数组（只含 activityId + assignmentId），保持模型返回
// 的顺序（提示词约定按到期日排序）。
func selectApprovalTasks(refs []ApprovalTaskRef, entries []approvalTaskIndexEntry) ([]ApprovalTaskRef, []string) {
	index := make(map[approvalBindingKey]struct{}, len(entries))
	for _, e := range entries {
		index[approvalBindingKey{activityID: e.activityID, assignmentID: e.assignmentID}] = struct{}{}
	}
	selected := make([]ApprovalTaskRef, 0, len(refs))
	seen := make(map[approvalBindingKey]struct{}, len(refs))
	var problems []string
	for i, ref := range refs {
		ref.ActivityID = strings.TrimSpace(ref.ActivityID)
		ref.AssignmentID = strings.TrimSpace(ref.AssignmentID)
		if ref.ActivityID == "" || ref.AssignmentID == "" {
			problems = append(problems, fmt.Sprintf("第 %d 条任务缺少 activityId 或 assignmentId", i+1))
			continue
		}
		key := approvalBindingKey{activityID: ref.ActivityID, assignmentID: ref.AssignmentID}
		if _, ok := index[key]; !ok {
			problems = append(problems, fmt.Sprintf(
				"第 %d 条任务不在 approvalTaskIndex 中：activityId=%s，assignmentId=%s",
				i+1, ref.ActivityID, ref.AssignmentID))
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		selected = append(selected, ref)
	}
	return selected, problems
}

// validateApprovalTaskSearchOutput 汇总一次模型输出的全部契约问题：正文缺失、
// 任务引用不在索引中。空切片表示可用。
func validateApprovalTaskSearchOutput(out *approvalTaskSearchOutput, entries []approvalTaskIndexEntry) []string {
	if out == nil {
		return []string{"输出为空"}
	}
	var problems []string
	if strings.TrimSpace(out.Message) == "" {
		problems = append(problems, "缺少必需的 message 字段（给用户的正文说明）")
	}
	if _, refProblems := selectApprovalTasks(out.Tasks, entries); len(refProblems) > 0 {
		problems = append(problems, refProblems...)
	}
	return problems
}

// parseAndValidateApprovalTaskSearch 解析并校验模型输出；解析失败与契约违约
// 都返回非空 problems，交由调用方决定是否纠正重试。
func parseAndValidateApprovalTaskSearch(
	content string,
	entries []approvalTaskIndexEntry,
) (*approvalTaskSearchOutput, []string) {
	out, err := parseApprovalTaskSearchOutput(content)
	if err != nil {
		return nil, []string{"输出不是合法的 JSON envelope：" + err.Error()}
	}
	return out, validateApprovalTaskSearchOutput(out, entries)
}

// repairApprovalTaskSearchOutput 在模型输出违反契约（非 JSON、缺正文、绑定
// 不在索引中）时，把原提示词、违约输出与违约说明一并回传模型做一次纠正重试。
// 裸调用没有引擎侧渲染，系统提示词先手动过一遍占位符（current_time 等）。
func repairApprovalTaskSearchOutput(
	ctx context.Context,
	chatModel chat.Chat,
	systemPrompt string,
	userContent string,
	badOutput string,
	problems []string,
) (*approvalTaskSearchOutput, error) {
	var sb strings.Builder
	sb.WriteString("你上一次的输出违反了输出契约：\n")
	for i, p := range problems {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, p))
	}
	sb.WriteString("请严格按系统提示词定义的输出格式重新输出完整 JSON（必须包含 message；")
	sb.WriteString("tasks 只能包含 approvalTaskIndex 中真实存在的 activityId/assignmentId，没有匹配时返回空数组）。")
	sb.WriteString("只输出一个纯 JSON 对象，禁止解释文字。")
	resp, err := chatModel.Chat(ctx, []chat.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userContent},
		{Role: "assistant", Content: badOutput},
		{Role: "user", Content: sb.String()},
	}, &chat.ChatOptions{Temperature: 0.1})
	if err != nil {
		return nil, err
	}
	return parseApprovalTaskSearchOutput(resp.Content)
}

// buildApprovalTaskSearchMetadata 组装任务筛选的结构化上下文：页面、完整
// 任务索引与日期基准。筛选规则与输出契约由 approval_search 智能体的
// system_prompt 定义，代码只注入数据事实。
//
// 这里刻意不注入 itemData：它的 approvalItem 是完整业务物件记录，可能非常
// 大，而任务筛选只需要索引里的绑定/单号/状态/日期字段；masterItem 的去重
// 由前端按 activityId+assignmentId 处理，不需要交给模型判断。
func buildApprovalTaskSearchMetadata(
	page *AIPageContext,
	approvalTaskIndex json.RawMessage,
) types.JSON {
	payload := struct {
		Page              *AIPageContext  `json:"page,omitempty"`
		ApprovalTaskIndex json.RawMessage `json:"approvalTaskIndex,omitempty"`
		DateContext       AIDateContext   `json:"dateContext"`
		DefaultTaskStatus string          `json:"defaultTaskStatus"`
	}{
		Page:              page,
		ApprovalTaskIndex: approvalTaskIndex,
		DateContext: AIDateContext{
			CurrentDate:       time.Now().Format("2006-01-02"),
			Timezone:          time.Local.String(),
			RelativeMonthMode: "calendar_month",
			ValueFormat:       "YYYY-MM-DD",
		},
		DefaultTaskStatus: "未签审",
	}
	b, _ := json.Marshal(payload)
	return types.JSON(b)
}

// buildApprovalTaskSearchPrompt 组装用户侧上下文（与主路径 Metadata 注入的
// 内容一致），供模型调用与纠正重试共用。
func buildApprovalTaskSearchPrompt(message string, metadata types.JSON, lang string) string {
	var sb strings.Builder
	sb.WriteString("[用户提供的结构化上下文]\n")
	sb.WriteString(string(metadata))
	sb.WriteString("\n\n用户请求：" + message)
	sb.WriteString("\n\n[回复语言：" + lang + "]")
	return sb.String()
}

// persistApprovalSearchReply 落库任务筛选轮的助手消息。只存正文纯文本，
// 不存 envelope JSON：历史若存 JSON，后续 normal 意图的主智能体会模仿该格式，
// 把普通回答也输出成 JSON 文本。客户端断开后仍持久化（对话历史需要完整）。
func (h *AIChatHandler) persistApprovalSearchReply(
	ctx context.Context,
	session *types.Session,
	req *AIChatRequest,
	agent *types.CustomAgent,
	message string,
	modelID string,
) {
	if session == nil || agent == nil {
		return
	}
	if modelID == "" {
		modelID = agent.Config.ModelID
	}
	persistCtx := context.WithValue(context.WithoutCancel(ctx), types.TenantIDContextKey, session.TenantID)
	if _, err := h.messageService.CreateMessage(persistCtx, &types.Message{
		SessionID:     session.ID,
		Role:          "assistant",
		Content:       message,
		RequestID:     req.RequestID,
		IsCompleted:   true,
		CreatedAt:     time.Now(),
		AgentID:       agent.ID,
		AgentTenantID: agent.TenantID,
		ModelID:       modelID,
	}); err != nil {
		logger.Warnf(ctx, "failed to persist approval task search reply: %v", err)
	}
}

// handleApprovalSearchMessage 处理签核任务筛选意图的消息：同步动态字段、
// 回填任务索引/itemData，调用 approval_search 智能体在索引中筛选任务，
// 校验绑定后发出单条 approval_task_selection 终止事件。该流程只在
// 「任务筛选开关开启 + 页面为 processApproval」时被触发（见 intent.go 的
// shouldApprovalSearch）。
func (h *AIChatHandler) handleApprovalSearchMessage(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	agent *types.CustomAgent,
	tenantID uint64,
) {
	// 主智能体参数仅保持 intentHandler 接口形状；任务筛选轮始终使用专用
	// approval_search 智能体（approval_search_agent_id → 主智能体）。

	searchAgent, err := h.resolveApprovalSearchAgent(ctx)
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, err.Error(), AIErrorCodeApprovalTask))
		return
	}

	session, err := h.getOrCreateSession(ctx, req.ConversationID, tenantID)
	if err != nil || session == nil {
		h.writeEvent(c, h.errorEvent(req, "failed to create session", AIErrorCodeApprovalTask))
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

	// 持久化用户消息，保证会话历史完整（任务筛选意图不携带附件）。
	if _, err := h.messageService.CreateMessage(ctx, &types.Message{
		SessionID:   session.ID,
		Role:        "user",
		Content:     req.Message,
		RequestID:   req.RequestID,
		IsCompleted: true,
		CreatedAt:   time.Now(),
	}); err != nil {
		h.writeEvent(c, h.errorEvent(req, "failed to persist user message: "+err.Error(), AIErrorCodeApprovalTask))
		return
	}

	entries, err := parseApprovalTaskIndex(req.ApprovalTaskIndex)
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, "invalid approvalTaskIndex payload: "+err.Error(), AIErrorCodeApprovalTask))
		return
	}

	// 没有任务索引时无法筛选：返回正常的终止事件（空列表 + 说明），而不是
	// 让模型凭空编造任务。
	if len(entries) == 0 {
		const noIndexMessage = "当前没有可用的签核任务索引，无法筛选任务。请刷新页面或重新进入流程签审页面后再试。"
		h.persistApprovalSearchReply(ctx, session, req, searchAgent, noIndexMessage, "")
		h.writeEvent(c, AIEvent{
			Version:        req.Version,
			Type:           AIEventApprovalTaskSelection,
			ConversationID: req.ConversationID,
			RequestID:      req.RequestID,
			Message:        noIndexMessage,
		})
		return
	}

	metadata := buildApprovalTaskSearchMetadata(page, req.ApprovalTaskIndex)
	lang := detectReplyLanguage(req.Message)
	systemPrompt := types.RenderPromptPlaceholders(searchAgent.Config.SystemPrompt, types.PlaceholderValues{
		"language": lang,
	})
	userContent := buildApprovalTaskSearchPrompt(req.Message, metadata, lang)

	// 模型优先级：请求体 model_id（须为可用的 KnowledgeQA 模型）> 专用智能体
	// 自己配置的模型 > ai_chat.model_id。前端在 processApproval 页面选择模型
	// 后应影响任务筛选；非法请求值记日志并回退，不中断请求。
	modelID := strings.TrimSpace(searchAgent.Config.ModelID)
	if mid := strings.TrimSpace(req.ModelID); mid != "" {
		if h.isKnowledgeQAModel(ctx, mid) {
			modelID = mid
		} else {
			logger.Warnf(ctx, "AI chat requested model_id %q is not a usable KnowledgeQA model; falling back", mid)
		}
	}
	if modelID == "" && h.config != nil && h.config.AIChat != nil {
		modelID = strings.TrimSpace(h.config.AIChat.ModelID)
	}
	chatModel, err := h.modelService.GetChatModel(ctx, modelID)
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, err.Error(), AIErrorCodeApprovalTask))
		return
	}

	logger.Infof(ctx, "[ai-chat] approval task search via model: agent=%s model=%s indexTasks=%d",
		searchAgent.ID, modelID, len(entries))
	resp, err := chatModel.Chat(ctx, []chat.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userContent},
	}, &chat.ChatOptions{Temperature: 0.1})
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, err.Error(), AIErrorCodeApprovalTask))
		return
	}
	logger.Infof(ctx, "[ai-chat] approval task search model output: %s", resp.Content)

	// 契约校验：可解析、正文非空、任务绑定都能在索引中找到。违约时回传
	// 违约输出做一次纠正重试，仍违约则按契约违约报错（不发出不可用列表）。
	result, problems := parseAndValidateApprovalTaskSearch(resp.Content, entries)
	if len(problems) > 0 {
		repaired, repairErr := repairApprovalTaskSearchOutput(ctx, chatModel, systemPrompt, userContent, resp.Content, problems)
		if repairErr != nil {
			logger.Warnf(ctx, "[ai-chat] approval task search repair failed: %v", repairErr)
		} else if repairedProblems := validateApprovalTaskSearchOutput(repaired, entries); len(repairedProblems) == 0 {
			result = repaired
			problems = nil
		} else {
			problems = repairedProblems
		}
	}
	if len(problems) > 0 {
		msg := "approval task search output violates the contract: " + strings.Join(problems, "；")
		h.writeEvent(c, h.errorEvent(req, msg, AIErrorCodeApprovalTask))
		return
	}

	// 绑定已在 validateApprovalTaskSearchOutput 中校验，这里输出可直接下发
	// 给前端的 selectedTasks（activityId + assignmentId）。
	selectedTasks, _ := selectApprovalTasks(result.Tasks, entries)

	h.persistApprovalSearchReply(ctx, session, req, searchAgent, result.Message, modelID)

	// 与搜索/签核草稿一致：基于会话数据与本轮正文生成本轮推荐问题。
	var suggestions []AISuggestionItem
	if recommendAgent, rerr := h.resolveRecommendAgent(ctx); rerr == nil {
		conversation := req.Message
		if strings.TrimSpace(result.Message) != "" {
			conversation += "\n\n" + result.Message
		}
		if s, serr := h.generateRecommendQuestions(ctx, recommendAgent, page, nil, nil, req.ItemData, conversation); serr == nil {
			suggestions = s
		}
	}

	// 单条 approval_task_selection 终止事件：正文说明复用 message，任务绑定
	// 在 selectedTasks（只含 activityId + assignmentId），推荐问题在
	// suggestions；其后不追加 answer_done，也不回显模型输出 envelope。
	if c.Request.Context().Err() != nil {
		return
	}
	h.writeEvent(c, AIEvent{
		Version:        req.Version,
		Type:           AIEventApprovalTaskSelection,
		ConversationID: req.ConversationID,
		RequestID:      req.RequestID,
		Message:        result.Message,
		SelectedTasks:  selectedTasks,
		Suggestions:    suggestions,
	})
}

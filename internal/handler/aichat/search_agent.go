package aichat

// 本文件实现搜索草稿流程的 AgentQA 接入：条件生成不再裸调用对话模型，而是
// 与 normal 流（agent_turn.go）一样把 search agent 送入智能体引擎，使其关联的
// 知识库（如 wiki 中录入的字段查询说明）可通过 knowledge_search 工具参与字段
// 判断，系统提示词的 {{current_time}} 等占位符也由引擎侧统一渲染。
// searchDraftTranslator 复用 aiStreamTranslator 的进度事件映射，但把完成路径
// 换成搜索草稿语义：最终答案不向客户端流式输出，而是解析为搜索条件草稿，
// 硬校验后发出 search_draft / need_clarification 事件。

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// searchDraftDeps 汇聚搜索草稿完成路径所需的全部依赖：生成条件所依据的
// 结构化上下文，以及输出契约违约时做纠正重试所需的智能体与提示词材料。
type searchDraftDeps struct {
	h            *AIChatHandler
	agent        *types.CustomAgent // 已应用 ai_chat.model_id 覆盖的 search agent
	message      string            // 用户原始请求（纠正重试的提示词材料）
	page         *AIPageContext
	searchFields []AISearchField
	searchBody   json.RawMessage
	itemData     json.RawMessage // 页面数据载荷，不限于 singleData（largeTable 可为表格数据）
	rules        *AIConditionRules
	lang         string
}

// repair 在模型输出缺少必需的 message 字段时，把原提示词与违约输出一并
// 回传给模型做一次纠正重试。文案措辞仍完全由提示词决定。裸调用没有引擎
// 侧渲染，系统提示词先手动过一遍占位符（current_time 等）。
func (d *searchDraftDeps) repair(ctx context.Context, badOutput string) (*AISearchResult, error) {
	chatModel, err := d.h.modelService.GetChatModel(ctx, d.agent.Config.ModelID)
	if err != nil {
		return nil, err
	}
	systemPrompt := types.RenderPromptPlaceholders(d.agent.Config.SystemPrompt, types.PlaceholderValues{
		"language": d.lang,
	})
	userContent := buildSearchPrompt(d.message, d.page, d.searchFields, d.itemData, d.rules, d.lang)
	resp, err := chatModel.Chat(ctx, []chat.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userContent},
		{Role: "assistant", Content: badOutput},
		{Role: "user", Content: "你上一次的输出缺少必需的 message 字段。请严格按系统提示词定义的输出格式重新输出完整 JSON（必须包含 message），只输出一个纯 JSON 对象，禁止解释文字。"},
	}, &chat.ChatOptions{Temperature: 0.1})
	if err != nil {
		return nil, err
	}
	return parseSearchResult(resp.Content)
}

// searchDraftTranslator 订阅智能体 EventBus：进度事件复用 normal 流的 SSE
// 映射（思考/工具调用/工具结果/引用，前端可见 search agent 检索 wiki 的过程），
// 最终答案仅本地累积，完成时走搜索草稿的解析/校验/事件输出。
type searchDraftTranslator struct {
	*aiStreamTranslator
	deps *searchDraftDeps
}

func newSearchDraftTranslator(
	base *aiStreamTranslator,
	deps *searchDraftDeps,
) *searchDraftTranslator {
	return &searchDraftTranslator{aiStreamTranslator: base, deps: deps}
}

// subscribe 注册搜索草稿流程的事件映射：进度事件复用 subscribeProgress，
// final answer 只累积不流式输出，complete 时落库助手消息并发出
// search_draft / need_clarification 终止事件。
func (t *searchDraftTranslator) subscribe(bus *event.EventBus) {
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
		logger.Infof(ctx, "[ai-chat] search draft model output: %s", final)

		// 智能体模式下知识引用通过 AgentCompleteData.KnowledgeRefs 提供，
		// 与独立 EventAgentReferences 事件累积的引用合并。
		var knowledgeRefs []*types.SearchResult
		for _, ref := range data.KnowledgeRefs {
			if sr, ok := ref.(*types.SearchResult); ok {
				knowledgeRefs = append(knowledgeRefs, sr)
			}
		}
		if len(knowledgeRefs) == 0 {
			t.mu.Lock()
			knowledgeRefs = append([]*types.SearchResult(nil), t.refs...)
			t.mu.Unlock()
		}

		// 契约校验：message 是输出格式的必填字段，缺失时回传违约输出做一次
		// 纠正重试，仍缺失则按契约违约报错。
		result, parseErr := parseSearchResult(final)
		if parseErr == nil && strings.TrimSpace(result.Message) == "" {
			repaired, repairErr := t.deps.repair(ctx, final)
			if repairErr == nil {
				result, parseErr = repaired, nil
			} else {
				logger.Warnf(ctx, "[ai-chat] search draft repair failed: %v", repairErr)
			}
		}

		// 落库助手消息：内容为（纠正后的）条件草稿 JSON，多轮 refinement
		// （如「改成去年」）可基于历史条件继续。
		updateCtx := context.WithValue(context.WithoutCancel(ctx), types.TenantIDContextKey, t.tenantID)
		t.asstMsg.Content = final
		t.asstMsg.KnowledgeReferences = types.References(knowledgeRefs)
		if steps, ok := data.AgentSteps.([]types.AgentStep); ok {
			t.asstMsg.AgentSteps = agenttools.SanitizeAgentStepsForStorage(steps)
		}
		t.asstMsg.IsCompleted = true
		t.asstMsg.UpdatedAt = time.Now()
		if err := t.h.messageService.UpdateMessage(updateCtx, t.asstMsg); err != nil {
			logger.Warnf(ctx, "failed to persist assistant message %s: %v", t.asstMsg.ID, err)
		}

		if parseErr != nil {
			t.write(AIEventError, "model output is not valid JSON: "+parseErr.Error(), true, nil)
			t.close()
			return nil
		}
		if strings.TrimSpace(result.Message) == "" {
			t.write(AIEventError, "model output missing required message field", true, nil)
			t.close()
			return nil
		}

		// 硬校验：字段/操作符/枚举值不合法则要求澄清，而不是直接返回不可用的草稿。
		// 事件需携带 unmatched 字段，直接构造完整 AIEvent 而非走 write（同样持写锁）。
		if unmatched := validateSearchResult(result, t.deps.searchFields, t.deps.rules); len(unmatched) > 0 {
			t.writeMu.Lock()
			t.h.writeEvent(t.c, AIEvent{
				Version:        t.req.Version,
				Type:           AIEventNeedClarification,
				ConversationID: t.req.ConversationID,
				RequestID:      t.req.RequestID,
				ActionID:       t.req.ActionID,
				Message:        "部分条件无法匹配，请确认或手动调整。",
				Unmatched:      unmatched,
			})
			t.writeMu.Unlock()
			t.close()
			return nil
		}

		// 同时基于会话数据和查询生成推荐问题。
		var suggestions []AISuggestionItem
		if recommendAgent, rerr := t.h.resolveRecommendAgent(ctx); rerr == nil {
			if s, serr := t.h.generateRecommendQuestions(ctx, recommendAgent, t.deps.page, t.deps.searchFields, t.deps.searchBody, t.deps.itemData, t.deps.message); serr == nil {
				suggestions = s
			}
		}

		needConfirm := true
		t.writeMu.Lock()
		t.h.writeEvent(t.c, AIEvent{
			Version:        t.req.Version,
			Type:           AIEventSearchDraft,
			ConversationID: t.req.ConversationID,
			RequestID:      t.req.RequestID,
			ActionID:       t.req.ActionID,
			Message:        result.Message,
			Conditions:     result.Conditions,
			SearchBody:     result.SearchBody,
			NeedConfirm:    &needConfirm,
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
		t.write(AIEventError, data.Error, true, map[string]interface{}{"stage": data.Stage})
		t.close()
		return nil
	})
}

// buildSearchMetadata 组装条件生成的结构化上下文。AgentQA 会把 Metadata 以
// [用户提供的结构化上下文] 追加到智能体查询之后（与 normal 流的
// buildAIMetadata 注入方式一致）。任务指令与输出契约由 search agent 自己的
// system_prompt 定义，代码不注入任务文案，避免与提示词形成双份契约。
// itemdata 不限于 singleData（largeTable 也可携带表格数据），与 normal 流一致
// 注入，供字段判断参考真实数据。
// dateContext 优先取前端两阶段路径的 conditionRules；快捷路径（start 已带
// searchFields）没有规则时由服务端用当前日期合成，search agent 的日期换算
// 规则（以 dateContext.currentDate 为基准）因此始终可用。
func buildSearchMetadata(
	page *AIPageContext,
	searchFields []AISearchField,
	searchBody json.RawMessage,
	itemData json.RawMessage,
	rules *AIConditionRules,
) types.JSON {
	payload := struct {
		Page         *AIPageContext  `json:"page,omitempty"`
		SearchFields []AISearchField `json:"searchFields,omitempty"`
		SearchBody   json.RawMessage `json:"searchBody,omitempty"`
		ItemData     json.RawMessage `json:"itemdata,omitempty"`
		DateContext  *AIDateContext  `json:"dateContext,omitempty"`
	}{
		Page:         page,
		SearchFields: searchFields,
		SearchBody:   searchBody,
		ItemData:     itemData,
	}
	if rules != nil && rules.DateContext.CurrentDate != "" {
		payload.DateContext = &rules.DateContext
	} else {
		payload.DateContext = &AIDateContext{
			CurrentDate: time.Now().Format("2006-01-02"),
			Timezone:    time.Local.String(),
			ValueFormat: "YYYY-MM-DD",
		}
	}
	b, _ := json.Marshal(payload)
	return types.JSON(b)
}

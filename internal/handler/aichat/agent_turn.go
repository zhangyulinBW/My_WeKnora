package aichat

// 本文件实现 normal 意图（普通问答）的处理：把用户消息送入真正的智能体引擎
// （AgentQA），并将每个智能体步骤（思考/工具调用/工具结果/引用/回答）以 AIEvent
// 流式输出。aiStreamTranslator 负责事件总线 -> SSE 协议的翻译与助手消息落库。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// handleAgentTurn 将用户消息送入真正的智能体引擎（AgentQA），
// 将每个智能体步骤（思考/工具调用/工具结果/引用/回答）以 AIEvent 流式输出。
// 助手消息预先创建，并在智能体完成后标记完成，从而持久化多轮历史和 AgentSteps。
// 对话模型遵循 ai_chat.model_id > 智能体自身 model_id 的优先级，与搜索草稿一致。
func (h *AIChatHandler) handleAgentTurn(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	agent *types.CustomAgent,
	tenantID uint64,
) {
	// 覆盖后的副本同时驱动 AgentQA 的模型解析与助手消息落库的 ModelID，
	// 两者必须是同一个值，否则日志/账单记的模型与实际调用的对不上。
	agent = h.applyModelOverride(agent)

	session, err := h.getOrCreateSession(ctx, req.ConversationID, tenantID)
	if err != nil || session == nil {
		h.writeEvent(c, h.errorEvent(req, "failed to create session"))
		return
	}

	// 若本轮请求未携带页面/筛选数据，则回填 start 阶段持久化的基础数据，
	// 保证后续 normal 对话仍能获得会话基础上下文（而非只依赖当轮请求）。
	page, searchFields, searchBody, itemData := h.loadStartContext(ctx, session.ID)
	if req.Page == nil {
		req.Page = page
	}
	if len(req.SearchFields) == 0 {
		req.SearchFields = searchFields
	}
	if len(req.SearchBody) == 0 {
		req.SearchBody = searchBody
	}
	if len(req.ItemData) == 0 {
		req.ItemData = itemData
	}

	if _, err := h.messageService.CreateMessage(ctx, &types.Message{
		SessionID:   session.ID,
		Role:        "user",
		Content:     req.Message,
		RequestID:   req.RequestID,
		IsCompleted: true,
		CreatedAt:   time.Now(),
	}); err != nil {
		h.writeEvent(c, h.errorEvent(req, "failed to persist user message: "+err.Error()))
		return
	}

	assistantMsg, err := h.messageService.CreateMessage(ctx, &types.Message{
		SessionID:        session.ID,
		Role:             "assistant",
		RequestID:        req.RequestID,
		IsCompleted:      false,
		CreatedAt:        time.Now(),
		AgentID:          agent.ID,
		AgentTenantID:    agent.TenantID,
		ModelID:          agent.Config.ModelID,
		ExecutionContext: buildAIExecutionContext(ctx, agent),
	})
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, "failed to persist assistant message: "+err.Error()))
		return
	}

	qaReq := &types.QARequest{
		Session:            session,
		Query:              req.Message,
		AssistantMessageID: assistantMsg.ID,
		CustomAgent:        agent,
		Metadata:           buildAIMetadata(req),
	}

	bus := event.NewEventBus()
	done := make(chan struct{})
	translator := newAIStreamTranslator(h, c, req, assistantMsg, session.TenantID, done)
	translator.subscribe(bus)

	go func() {
		defer translator.close()
		// Most runtime failures are emitted by AgentQA onto the event bus. Some
		// setup failures (for example an enabled knowledge_search tool without a
		// rerank model) occur before the engine exists and are returned directly.
		// Do not discard those errors: the HTTP status is already 200 for this
		// SSE response, so the client can only learn about the failure from a
		// terminal stream error frame.
		if err := h.sessionService.AgentQA(ctx, qaReq, bus); err != nil {
			translator.write(AIEventError, err.Error(), true, map[string]interface{}{
				"stage": "agent_execution",
			})
		}
	}()

	select {
	case <-done:
	case <-c.Request.Context().Done():
	}
}

// aiStreamTranslator 订阅智能体 EventBus，将每个事件转换为 AIEvent SSE 协议，
// 同时累积助手消息状态（最终回答、引用、智能体步骤），以便完成时持久化。
type aiStreamTranslator struct {
	h        *AIChatHandler
	c        *gin.Context
	req      *AIChatRequest
	asstMsg  *types.Message
	tenantID uint64
	done     chan struct{}

	mu        sync.Mutex
	closeOnce sync.Once
	answered  bool
	final     strings.Builder
	refs      []*types.SearchResult
}

func newAIStreamTranslator(
	h *AIChatHandler,
	c *gin.Context,
	req *AIChatRequest,
	asstMsg *types.Message,
	tenantID uint64,
	done chan struct{},
) *aiStreamTranslator {
	return &aiStreamTranslator{
		h:        h,
		c:        c,
		req:      req,
		asstMsg:  asstMsg,
		tenantID: tenantID,
		done:     done,
	}
}

func (t *aiStreamTranslator) close() {
	t.closeOnce.Do(func() { close(t.done) })
}

func (t *aiStreamTranslator) write(typ, content string, done bool, data map[string]interface{}) {
	// 若客户端已断开，则不再写入已结束的响应写入器（避免后台协程写入）。
	if t.c.Request.Context().Err() != nil {
		return
	}
	ev := AIEvent{
		Version:        t.req.Version,
		Type:           typ,
		ConversationID: t.req.ConversationID,
		RequestID:      t.req.RequestID,
		Content:        content,
		Done:           done,
		Data:           data,
	}
	if typ == AIEventError {
		// SSE has already started, so an upstream HTTP 429 cannot change this
		// request's HTTP status. Emit the standard stream error shape as well as
		// the AI-chat fields, allowing clients to surface quota/reset messages.
		ev.ID = t.req.RequestID
		ev.ResponseType = AIEventError
		if ev.Data == nil {
			ev.Data = make(map[string]interface{})
		}
		ev.Data["error"] = content
	}
	t.h.writeEvent(t.c, ev)
}

// writeAnswerDone 发出终止的 answer_done 事件，携带会话和助手消息 ID，
// 便于客户端拉取后续推荐问题。
func (t *aiStreamTranslator) writeAnswerDone() {
	t.write(AIEventAnswerDone, "", true, map[string]interface{}{
		"session_id":           t.asstMsg.SessionID,
		"assistant_message_id": t.asstMsg.ID,
	})
}

// writeSuggestions 发出终止的 suggestions 事件，作为 normal 对话流末尾的推荐问题。
func (t *aiStreamTranslator) writeSuggestions(suggestions []AISuggestionItem) {
	if t.c.Request.Context().Err() != nil {
		return
	}
	t.h.writeEvent(t.c, AIEvent{
		Version:        t.req.Version,
		Type:           AIEventSuggestions,
		ConversationID: t.req.ConversationID,
		RequestID:      t.req.RequestID,
		Suggestions:    suggestions,
	})
}

// subscribe 将智能体事件总线上的每个事件映射为对应的 SSE 事件。
func (t *aiStreamTranslator) subscribe(bus *event.EventBus) {
	bus.On(event.EventAgentThought, func(ctx context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentThoughtData)
		if !ok {
			return nil
		}
		t.write(AIEventThinking, data.Content, data.Done, map[string]interface{}{"iteration": data.Iteration})
		return nil
	})

	bus.On(event.EventAgentToolCall, func(ctx context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentToolCallData)
		if !ok {
			return nil
		}
		t.write(AIEventToolCall, fmt.Sprintf("Calling tool: %s", data.ToolName), false, map[string]interface{}{
			"tool_name":    data.ToolName,
			"arguments":    data.Arguments,
			"tool_call_id": data.ToolCallID,
		})
		return nil
	})

	bus.On(event.EventAgentToolResult, func(ctx context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentToolResultData)
		if !ok {
			return nil
		}
		content := agenttools.StreamContentForToolResult(data.ToolName, data.Success, data.Error, data.Data)
		t.write(AIEventToolResult, content, false, map[string]interface{}{
			"tool_name":   data.ToolName,
			"success":     data.Success,
			"duration_ms": data.Duration,
		})
		return nil
	})

	bus.On(event.EventAgentReferences, func(ctx context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentReferencesData)
		if !ok {
			return nil
		}
		t.mu.Lock()
		if refs, ok := data.References.([]*types.SearchResult); ok {
			t.refs = append(t.refs, refs...)
		}
		t.mu.Unlock()
		t.write(AIEventReferences, "", false, map[string]interface{}{"references": data.References})
		return nil
	})

	bus.On(event.EventAgentReflection, func(ctx context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentReflectionData)
		if !ok {
			return nil
		}
		t.write(AIEventReflection, data.Content, data.Done, nil)
		return nil
	})

	bus.On(event.EventAgentFinalAnswer, func(ctx context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentFinalAnswerData)
		if !ok {
			return nil
		}
		t.mu.Lock()
		if data.Content != "" {
			t.answered = true
			t.final.WriteString(data.Content)
		}
		t.mu.Unlock()
		if data.Content != "" {
			t.write(AIEventAnswerChunk, data.Content, false, nil)
		}
		if data.Done {
			t.writeAnswerDone()
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
		answered := t.answered
		refs := append([]*types.SearchResult(nil), t.refs...)
		t.mu.Unlock()

		if !answered && data.FinalAnswer != "" {
			t.write(AIEventAnswerChunk, data.FinalAnswer, false, nil)
			t.writeAnswerDone()
			final = data.FinalAnswer
		}

		// 智能体模式下，知识引用通过 AgentCompleteData.KnowledgeRefs 提供
		// （而非独立的 EventAgentReferences）。将其与已累积的引用合并。
		var knowledgeRefs []*types.SearchResult
		for _, ref := range data.KnowledgeRefs {
			if sr, ok := ref.(*types.SearchResult); ok {
				knowledgeRefs = append(knowledgeRefs, sr)
			}
		}
		if len(knowledgeRefs) == 0 {
			knowledgeRefs = refs
		}
		if len(knowledgeRefs) > 0 {
			t.write(AIEventReferences, "", false, map[string]interface{}{"references": types.References(knowledgeRefs)})
		}

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

		// 回答完成后触发后续问题生成（回答后推荐问题）。
		// 使用 WithoutCancel 以便在客户端断开后仍能继续执行。
		if t.h.suggestionService != nil {
			bgCtx := context.WithoutCancel(ctx)
			go func() {
				if _, err := t.h.suggestionService.EnsureFollowUps(bgCtx, t.asstMsg.SessionID, t.asstMsg.ID, false); err != nil {
					logger.Warnf(bgCtx, "follow-up suggestion generation failed for message %s: %v", t.asstMsg.ID, err)
				}
			}()
		}

		// 在 normal 对话流末尾追加推荐问题：基于会话基础数据与当前对话生成候选问题。
		// 使用回填后的 req（含 start 基础数据），以便推荐智能体获得完整上下文。
		if recommendAgent, rerr := t.h.resolveRecommendAgent(ctx); rerr == nil {
			conversation := t.req.Message
			if final != "" {
				conversation += "\n\n" + final
			}
			if suggestions, serr := t.h.generateRecommendQuestions(ctx, recommendAgent, t.req.Page, t.req.SearchFields, t.req.SearchBody, t.req.ItemData, conversation); serr == nil && len(suggestions) > 0 {
				t.writeSuggestions(suggestions)
			}
		}

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

// buildAIMetadata 将结构化的页面/搜索上下文组装为 types.JSON，
// 以便智能体通过 QARequest.Metadata 接收（直接注入智能体上下文，不会持久化到用户消息）。
func buildAIMetadata(req *AIChatRequest) types.JSON {
	ctxObj := map[string]interface{}{}
	if req.Page != nil {
		ctxObj["page"] = req.Page
	}
	if len(req.SearchFields) > 0 {
		ctxObj["searchFields"] = req.SearchFields
	}
	if len(req.SearchBody) > 0 {
		ctxObj["searchBody"] = req.SearchBody
	}
	if len(req.ItemData) > 0 {
		ctxObj["itemdata"] = req.ItemData
	}
	b, _ := json.Marshal(ctxObj)
	return types.JSON(b)
}

// buildAIExecutionContext 将智能体的后续推荐配置和语言环境快照到助手消息上，
// 以便推荐服务生成基于回答的推荐问题（“回答后推荐问题”选项）。
func buildAIExecutionContext(ctx context.Context, agent *types.CustomAgent) types.MessageExecutionContext {
	locale, _ := types.LanguageFromContext(ctx)
	if locale == "" {
		locale = types.DefaultLanguage()
	}
	ec := types.MessageExecutionContext{Locale: locale}
	if agent != nil && agent.Config.QuestionSuggestions != nil {
		if encoded, err := json.Marshal(agent.Config.QuestionSuggestions); err == nil {
			var suggestions types.QuestionSuggestionConfig
			if json.Unmarshal(encoded, &suggestions) == nil {
				ec.QuestionSuggestions = &suggestions
			}
		}
	}
	return ec
}

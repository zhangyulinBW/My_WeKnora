package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// defaultAIAgentID is the OPLink agent used when WEKNORA_AI_AGENT_ID is unset.
// Deployment-specific: override via the WEKNORA_AI_AGENT_ID env var.
const defaultAIAgentID = "6a188aae-b3fb-45c7-b59b-1d0a4620a113"

// pendingAction records the state of a recognised-but-not-yet-resolved search
// task, keyed by actionId. v1 keeps this in memory; an in-flight task does not
// survive a restart.
type pendingAction struct {
	ConversationID string
	RequestID      string
	Message        string
	Page           *AIPageContext
}

// AIChatHandler implements the custom /api/v1/ai/chat protocol.
type AIChatHandler struct {
	sessionService     interfaces.SessionService
	messageService     interfaces.MessageService
	modelService       interfaces.ModelService
	customAgentService interfaces.CustomAgentService
	config             *config.Config

	mu      sync.Mutex
	actions map[string]*pendingAction
}

// NewAIChatHandler creates the AI chat handler.
func NewAIChatHandler(
	sessionService interfaces.SessionService,
	messageService interfaces.MessageService,
	modelService interfaces.ModelService,
	customAgentService interfaces.CustomAgentService,
	cfg *config.Config,
) *AIChatHandler {
	return &AIChatHandler{
		sessionService:     sessionService,
		messageService:     messageService,
		modelService:       modelService,
		customAgentService: customAgentService,
		config:             cfg,
		actions:            make(map[string]*pendingAction),
	}
}

// Chat godoc
// @Summary      AI 自定义聊天
// @Description  自定义 AI 接口协议（OPLink），支持普通问答与大表格搜索任务，SSE 流式响应
// @Tags         AI
// @Accept       json
// @Produce      text/event-stream
// @Param        request  body      AIChatRequest           true  "AI 聊天请求"
// @Success      200      {object}  map[string]interface{}  "AI 聊天结果（SSE流）"
// @Failure      400      {object}  map[string]interface{}  "请求参数错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /ai/chat [post]
func (h *AIChatHandler) Chat(c *gin.Context) {
	ctx := c.Request.Context()

	var req AIChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if req.Version == "" {
		req.Version = AIProtocolVersion
	}
	if req.ConversationID == "" || req.RequestID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "conversationId and requestId are required"})
		return
	}

	tenantID := types.MustTenantIDFromContext(ctx)

	h.setSSEHeaders(c)

	agent, err := h.resolveAgent(ctx)
	if err != nil {
		h.writeEvent(c, h.errorEvent(&req, err.Error()))
		return
	}

	switch req.Type {
	case AIRequestTypeMessage:
		h.handleMessage(ctx, c, &req, agent, tenantID)
	case AIRequestTypeSearchContext:
		// The search-context stage still generates structured JSON with the main
		// agent's model directly; the message path uses AgentQA instead.
		modelID := agent.Config.ModelID
		if h.config != nil && h.config.AIChat != nil && h.config.AIChat.ModelID != "" {
			modelID = h.config.AIChat.ModelID
		}
		chatModel, err := h.modelService.GetChatModel(ctx, modelID)
		if err != nil {
			h.writeEvent(c, h.errorEvent(&req, "model unavailable: "+err.Error()))
			return
		}
		h.handleSearchContext(ctx, c, &req, agent, chatModel)
	default:
		h.writeEvent(c, h.errorEvent(&req, "unknown type: "+req.Type))
	}
}

// resolveAgent loads the OPLink agent whose system prompt + model drive the
// endpoint. The agent id is resolved from config.yaml (ai_chat.agent_id), then
// the WEKNORA_AI_AGENT_ID env var, then a built-in default.
func (h *AIChatHandler) resolveAgent(ctx context.Context) (*types.CustomAgent, error) {
	id := defaultAIAgentID
	if h.config != nil && h.config.AIChat != nil && h.config.AIChat.AgentID != "" {
		id = h.config.AIChat.AgentID
	} else if env := strings.TrimSpace(os.Getenv("WEKNORA_AI_AGENT_ID")); env != "" {
		id = env
	}
	agent, err := h.customAgentService.GetAgentByID(ctx, id)
	if err != nil || agent == nil {
		return nil, fmt.Errorf("AI agent %s not found", id)
	}
	return agent, nil
}

// resolveIntentAgent loads the intent-analysis agent used for intent
// classification. The id comes from ai_chat.intent_agent_id (same custom_agents
// table as the main agent) and falls back to the main agent when unset.
func (h *AIChatHandler) resolveIntentAgent(ctx context.Context) (*types.CustomAgent, error) {
	if h.config != nil && h.config.AIChat != nil && h.config.AIChat.IntentAgentID != "" {
		agent, err := h.customAgentService.GetAgentByID(ctx, h.config.AIChat.IntentAgentID)
		if err != nil || agent == nil {
			return nil, fmt.Errorf("intent agent %s not found", h.config.AIChat.IntentAgentID)
		}
		return agent, nil
	}
	return h.resolveAgent(ctx)
}

// handleMessage classifies the message via the intent-analysis agent, persists
// the intent, then dispatches to the search-task or agent-answer flow.
func (h *AIChatHandler) handleMessage(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	agent *types.CustomAgent,
	tenantID uint64,
) {
	intent, err := h.classifyIntent(ctx, req.Message, req.Page, agent)
	if err != nil {
		logger.Warnf(ctx, "AI chat intent classification failed, defaulting to normal: %v", err)
		intent = "normal"
	}
	h.persistIntent(ctx, req, tenantID, intent)

	if intent == "search" {
		h.handleSearchMessage(ctx, c, req, tenantID)
		return
	}
	h.handleAgentTurn(ctx, c, req, agent, tenantID)
}

// classifyIntent returns "search" or "normal" using the intent-analysis agent's
// system prompt + model (resolved from ai_chat.intent_agent_id). The model falls
// back to ai_chat.model_id and then the main agent's model when the intent agent
// does not declare its own.
func (h *AIChatHandler) classifyIntent(
	ctx context.Context,
	message string,
	page *AIPageContext,
	mainAgent *types.CustomAgent,
) (string, error) {
	intentAgent, err := h.resolveIntentAgent(ctx)
	if err != nil {
		return "", err
	}

	modelID := intentAgent.Config.ModelID
	if modelID == "" && h.config != nil && h.config.AIChat != nil && h.config.AIChat.ModelID != "" {
		modelID = h.config.AIChat.ModelID
	}
	if modelID == "" && mainAgent != nil {
		modelID = mainAgent.Config.ModelID
	}
	chatModel, err := h.modelService.GetChatModel(ctx, modelID)
	if err != nil {
		return "", err
	}

	user := "用户消息：" + message
	if page != nil && page.PageType != "" {
		user += "\n页面类型：" + page.PageType
	}
	resp, err := chatModel.Chat(ctx, []chat.Message{
		{Role: "system", Content: intentAgent.Config.SystemPrompt},
		{Role: "user", Content: user},
	}, &chat.ChatOptions{})
	if err != nil {
		return "", err
	}
	content := strings.ToLower(strings.TrimSpace(resp.Content))
	if strings.Contains(content, "search") {
		return "search", nil
	}
	return "normal", nil
}

// persistIntent stores the classified intent as a dedicated "intent" message in
// the same messages table as the rest of the conversation. History builders only
// consume user/assistant roles, so it stays queryable without polluting LLM context.
func (h *AIChatHandler) persistIntent(ctx context.Context, req *AIChatRequest, tenantID uint64, intent string) {
	session, err := h.getOrCreateSession(ctx, req.ConversationID, tenantID)
	if err != nil || session == nil {
		return
	}
	content, _ := json.Marshal(map[string]string{"intent": intent})
	_, _ = h.messageService.CreateMessage(ctx, &types.Message{
		SessionID:   session.ID,
		Role:        "intent",
		Content:     string(content),
		RequestID:   req.RequestID,
		IsCompleted: true,
		CreatedAt:   time.Now(),
	})
}

// handleSearchMessage is stage one of the search flow: recognise the task and
// ask the frontend for searchFields + conditionRules.
func (h *AIChatHandler) handleSearchMessage(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	tenantID uint64,
) {
	actionID := uuid.New().String()
	h.mu.Lock()
	h.actions[actionID] = &pendingAction{
		ConversationID: req.ConversationID,
		RequestID:      req.RequestID,
		Message:        req.Message,
		Page:           req.Page,
	}
	h.mu.Unlock()

	// Persist the user message so the conversation history stays complete.
	if session, err := h.getOrCreateSession(ctx, req.ConversationID, tenantID); err == nil && session != nil {
		_, _ = h.messageService.CreateMessage(ctx, &types.Message{
			SessionID:   session.ID,
			Role:        "user",
			Content:     req.Message,
			RequestID:   req.RequestID,
			IsCompleted: true,
			CreatedAt:   time.Now(),
		})
	}

	h.writeEvent(c, AIEvent{
		Version:        req.Version,
		Type:           AIEventNeedSearchContext,
		ConversationID: req.ConversationID,
		RequestID:      req.RequestID,
		ActionID:       actionID,
		Required:       []string{"searchFields", "conditionRules"},
	})
}

// handleSearchContext is stage two: generate conditions + searchBody, validate,
// then return a search_draft or need_clarification.
func (h *AIChatHandler) handleSearchContext(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	agent *types.CustomAgent,
	chatModel chat.Chat,
) {
	h.mu.Lock()
	action := h.actions[req.ActionID]
	h.mu.Unlock()
	if action == nil {
		h.writeEvent(c, h.errorEvent(req, "action not found: "+req.ActionID))
		return
	}

	lang := detectReplyLanguage(action.Message)
	userContent := buildSearchPrompt(action.Message, action.Page, req.SearchFields, req.ConditionRules, lang)
	msgs := []chat.Message{
		{Role: "system", Content: agent.Config.SystemPrompt},
		{Role: "user", Content: userContent},
	}

	resp, err := chatModel.Chat(ctx, msgs, &chat.ChatOptions{Temperature: 0.1})
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, "model call failed: "+err.Error()))
		return
	}

	result, err := parseSearchResult(resp.Content)
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, "model output is not valid JSON: "+err.Error()))
		return
	}

	unmatched := validateSearchResult(result, req.SearchFields, req.ConditionRules)
	if len(unmatched) > 0 {
		h.writeEvent(c, AIEvent{
			Version:        req.Version,
			Type:           AIEventNeedClarification,
			ConversationID: req.ConversationID,
			RequestID:      req.RequestID,
			ActionID:       req.ActionID,
			Message:        "部分条件无法匹配，请确认或手动调整。",
			Unmatched:      unmatched,
		})
		return
	}

	needConfirm := true
	h.writeEvent(c, AIEvent{
		Version:        req.Version,
		Type:           AIEventSearchDraft,
		ConversationID: req.ConversationID,
		RequestID:      req.RequestID,
		ActionID:       req.ActionID,
		Message:        fmt.Sprintf("已整理出 %d 条搜索条件，请确认。", len(result.Conditions)),
		Conditions:     result.Conditions,
		SearchBody:     result.SearchBody,
		NeedConfirm:    &needConfirm,
	})
}

// handleAgentTurn runs the user message through the real agent engine (AgentQA),
// streaming each agent step (thinking / tool_call / tool_result / references /
// answer) as an AIEvent. The assistant message is pre-created and completed once
// the agent finishes, so multi-turn history and AgentSteps persist.
func (h *AIChatHandler) handleAgentTurn(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	agent *types.CustomAgent,
	tenantID uint64,
) {
	session, err := h.getOrCreateSession(ctx, req.ConversationID, tenantID)
	if err != nil || session == nil {
		h.writeEvent(c, h.errorEvent(req, "failed to create session"))
		return
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
		SessionID:   session.ID,
		Role:        "assistant",
		RequestID:   req.RequestID,
		IsCompleted: false,
		CreatedAt:   time.Now(),
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
		_ = h.sessionService.AgentQA(ctx, qaReq, bus)
	}()

	select {
	case <-done:
	case <-c.Request.Context().Done():
	}
}

// aiStreamTranslator subscribes to the agent EventBus and translates each event
// into the AIEvent SSE protocol while accumulating the assistant message state
// (final answer, references, agent steps) for persistence on completion.
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
	// Bail once the client has disconnected so we never write to a finalized
	// response writer from the background agent goroutine.
	if t.c.Request.Context().Err() != nil {
		return
	}
	t.h.writeEvent(t.c, AIEvent{
		Version:        t.req.Version,
		Type:           typ,
		ConversationID: t.req.ConversationID,
		RequestID:      t.req.RequestID,
		Content:        content,
		Done:           done,
		Data:           data,
	})
}

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
			t.write(AIEventAnswerDone, "", true, nil)
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
			t.write(AIEventAnswerDone, "", true, nil)
			final = data.FinalAnswer
		}

		// Agent mode delivers knowledge references via AgentCompleteData.KnowledgeRefs
		// (not a standalone EventAgentReferences). Combine with any accumulated refs.
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

// buildAIMetadata assembles the structured page/search context into types.JSON
// so the agent receives it via QARequest.Metadata (injected verbatim into the
// agent context, never persisted on the user message).
func buildAIMetadata(req *AIChatRequest) types.JSON {
	ctxObj := map[string]interface{}{}
	if req.Page != nil {
		ctxObj["page"] = req.Page
	}
	if len(req.SearchBody) > 0 {
		ctxObj["searchBody"] = req.SearchBody
	}
	b, _ := json.Marshal(ctxObj)
	return types.JSON(b)
}

// getOrCreateSession maps the opaque conversationId to a WeKnora session,
// creating it on first use. The session owner is derived from
// SessionOwnerIDFromContext so it matches the scope used by CreateMessage's
// session lookup. A pre-existing session created with a mismatched owner (e.g.
// by the earlier synthetic-user bug) is repaired in place so the scoped lookup
// succeeds on every subsequent turn.
func (h *AIChatHandler) getOrCreateSession(
	ctx context.Context,
	conversationID string,
	tenantID uint64,
) (*types.Session, error) {
	ownerID := types.SessionOwnerIDFromContext(ctx)
	if session, err := h.sessionService.GetSessionByID(ctx, tenantID, conversationID); err == nil && session != nil {
		if session.UserID != ownerID {
			if err := h.sessionService.SetSessionOwnerID(ctx, tenantID, conversationID, ownerID); err == nil {
				session.UserID = ownerID
			}
		}
		return session, nil
	}
	return h.sessionService.CreateSession(ctx, &types.Session{
		ID:       conversationID,
		TenantID: tenantID,
		UserID:   ownerID,
	})
}

// setSSEHeaders writes the streaming response headers.
func (h *AIChatHandler) setSSEHeaders(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
}

// writeEvent serialises one SSE event in the `data: {json}\n\n` format.
func (h *AIChatHandler) writeEvent(c *gin.Context, ev AIEvent) {
	b, _ := json.Marshal(ev)
	_, _ = c.Writer.WriteString("data: " + string(b) + "\n\n")
	c.Writer.Flush()
}

// errorEvent builds a terminal error event.
func (h *AIChatHandler) errorEvent(req *AIChatRequest, msg string) AIEvent {
	return AIEvent{
		Version:        req.Version,
		Type:           AIEventError,
		ConversationID: req.ConversationID,
		RequestID:      req.RequestID,
		Message:        msg,
	}
}

// buildSearchPrompt assembles the user-side context for condition generation.
func buildSearchPrompt(
	message string,
	page *AIPageContext,
	searchFields []AISearchField,
	rules *AIConditionRules,
	lang string,
) string {
	ctxObj := map[string]interface{}{}
	if page != nil {
		ctxObj["page"] = page
	}
	if searchFields != nil {
		ctxObj["searchFields"] = searchFields
	}
	if rules != nil {
		ctxObj["dateContext"] = rules.DateContext
	}
	ctxJSON, _ := json.Marshal(ctxObj)

	var sb strings.Builder
	sb.WriteString("[用户提供的结构化上下文]\n")
	sb.WriteString(string(ctxJSON))
	sb.WriteString("\n\n")
	sb.WriteString("用户请求：" + message)
	sb.WriteString("\n\n[回复语言：" + lang + "]")
	sb.WriteString("\n\n这是一次搜索任务。请严格按系统提示词中的「任务三」输出，只输出一个纯 JSON 对象（以 { 开头、以 } 结尾），禁止使用 markdown 代码块、禁止任何解释文字。")
	return sb.String()
}

// parseSearchResult extracts and parses the model's JSON output.
func parseSearchResult(content string) (*AISearchResult, error) {
	jsonStr := extractJSONObject(content)
	var result AISearchResult
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		preview := content
		if len(preview) > 300 {
			preview = preview[:300] + "..."
		}
		return nil, fmt.Errorf("%w (raw model output: %s)", err, preview)
	}
	return &result, nil
}

// extractJSONObject strips markdown fences and returns the outermost JSON object.
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start != -1 && end > start {
		return s[start : end+1]
	}
	return s
}

// detectReplyLanguage classifies the query as Traditional Chinese, Simplified
// Chinese, or English so the reply follows the user's language.
func detectReplyLanguage(query string) string {
	const traditionalOnly = "這個頁麼數據嗎們說為對時單價報條資訊現請幫辦體裡點會還來國學間開關問見讓給從後過種樣長門頭實應務區號語詢發"
	if strings.ContainsAny(query, traditionalOnly) {
		return "繁体中文"
	}
	hasCJK, hasLatin := false, false
	for _, r := range query {
		switch {
		case r >= 0x4E00 && r <= 0x9FFF:
			hasCJK = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			hasLatin = true
		}
	}
	if !hasCJK && hasLatin {
		return "English"
	}
	return "简体中文"
}

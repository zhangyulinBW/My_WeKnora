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

	"github.com/Tencent/WeKnora/internal/config"
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
	userID, _ := types.UserIDFromContext(ctx)

	h.setSSEHeaders(c)

	agent, err := h.resolveAgent(ctx)
	if err != nil {
		h.writeEvent(c, h.errorEvent(&req, err.Error()))
		return
	}
	modelID := agent.Config.ModelID
	if h.config != nil && h.config.AIChat != nil && h.config.AIChat.ModelID != "" {
		modelID = h.config.AIChat.ModelID
	}
	chatModel, err := h.modelService.GetChatModel(ctx, modelID)
	if err != nil {
		h.writeEvent(c, h.errorEvent(&req, "model unavailable: "+err.Error()))
		return
	}

	switch req.Type {
	case AIRequestTypeMessage:
		h.handleMessage(ctx, c, &req, agent, chatModel, tenantID, userID)
	case AIRequestTypeSearchContext:
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

// handleMessage classifies the message and dispatches to the normal-answer or
// search-task flow.
func (h *AIChatHandler) handleMessage(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	agent *types.CustomAgent,
	chatModel chat.Chat,
	tenantID uint64,
	userID string,
) {
	intent, err := h.classifyIntent(ctx, chatModel, req.Message, req.Page)
	if err != nil {
		logger.Warnf(ctx, "AI chat intent classification failed, defaulting to normal: %v", err)
		intent = "normal"
	}

	if intent == "search" {
		h.handleSearchMessage(ctx, c, req, tenantID, userID)
		return
	}
	h.handleNormalAnswer(ctx, c, req, agent, chatModel, tenantID, userID)
}

// classifyIntent returns "search" or "normal" using the agent's model.
func (h *AIChatHandler) classifyIntent(
	ctx context.Context,
	chatModel chat.Chat,
	message string,
	page *AIPageContext,
) (string, error) {
	sys := "你是意图分类器。判断用户消息是「要执行一次新的搜索/筛选」（search）还是「普通问答/询问当前页面或查询状态」（normal）。" +
		"\n- search：用户要你【执行】搜索、找出符合条件的数据。例：「帮我找出金额大于10万的单」「查询状态为已发布的单」。" +
		"\n- normal：用户在【询问】页面是什么、当前正在查询什么、能查什么，或普通对话。例：「这个页面是做什么的？」「目前查询的是什么数据？」「能查什么字段？」。" +
		"\n注意：含「是什么」「目前」「当前」「能查」等询问性表述的是 normal，即使出现「查询」二字。只输出一个词：search 或 normal。"
	user := "用户消息：" + message
	if page != nil && page.PageType != "" {
		user += "\n页面类型：" + page.PageType
	}
	resp, err := chatModel.Chat(ctx, []chat.Message{
		{Role: "system", Content: sys},
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

// handleSearchMessage is stage one of the search flow: recognise the task and
// ask the frontend for searchFields + conditionRules.
func (h *AIChatHandler) handleSearchMessage(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	tenantID uint64,
	userID string,
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
	if session, err := h.getOrCreateSession(ctx, req.ConversationID, tenantID, userID); err == nil && session != nil {
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

// handleNormalAnswer streams a plain-text answer (with multi-turn history).
func (h *AIChatHandler) handleNormalAnswer(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	agent *types.CustomAgent,
	chatModel chat.Chat,
	tenantID uint64,
	userID string,
) {
	session, err := h.getOrCreateSession(ctx, req.ConversationID, tenantID, userID)
	if err != nil || session == nil {
		h.writeEvent(c, h.errorEvent(req, "failed to create session"))
		return
	}

	lang := detectReplyLanguage(req.Message)
	userContent := buildNormalUserPrompt(req.Message, req.Page, req.SearchBody, lang)

	msgs := []chat.Message{{Role: "system", Content: agent.Config.SystemPrompt}}
	msgs = append(msgs, h.buildHistory(ctx, session.ID, 10)...)
	msgs = append(msgs, chat.Message{Role: "user", Content: userContent})

	opts := &chat.ChatOptions{
		Temperature:         agent.Config.Temperature,
		MaxCompletionTokens: agent.Config.MaxCompletionTokens,
		Thinking:            agent.Config.Thinking,
	}

	stream, err := chatModel.ChatStream(ctx, msgs, opts)
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, "model call failed: "+err.Error()))
		return
	}

	_, _ = h.messageService.CreateMessage(ctx, &types.Message{
		SessionID:   session.ID,
		Role:        "user",
		Content:     req.Message,
		RequestID:   req.RequestID,
		IsCompleted: true,
		CreatedAt:   time.Now(),
	})

	var answer strings.Builder
	for resp := range stream {
		if resp.Content != "" {
			answer.WriteString(resp.Content)
			h.writeEvent(c, AIEvent{
				Version:        req.Version,
				Type:           AIEventAnswerChunk,
				ConversationID: req.ConversationID,
				RequestID:      req.RequestID,
				Content:        resp.Content,
			})
		}
	}

	_, _ = h.messageService.CreateMessage(ctx, &types.Message{
		SessionID:   session.ID,
		Role:        "assistant",
		Content:     answer.String(),
		RequestID:   req.RequestID,
		IsCompleted: true,
		CreatedAt:   time.Now(),
	})

	h.writeEvent(c, AIEvent{
		Version:        req.Version,
		Type:           AIEventAnswerDone,
		ConversationID: req.ConversationID,
		RequestID:      req.RequestID,
	})
}

// getOrCreateSession maps the opaque conversationId to a WeKnora session,
// creating it on first use.
func (h *AIChatHandler) getOrCreateSession(
	ctx context.Context,
	conversationID string,
	tenantID uint64,
	userID string,
) (*types.Session, error) {
	if session, err := h.sessionService.GetSessionByID(ctx, tenantID, conversationID); err == nil && session != nil {
		return session, nil
	}
	return h.sessionService.CreateSession(ctx, &types.Session{
		ID:       conversationID,
		TenantID: tenantID,
		UserID:   userID,
	})
}

// buildHistory converts recent persisted messages into chat history.
// GetRecentMessagesBySession returns messages oldest-first, which is the order
// the model expects.
func (h *AIChatHandler) buildHistory(ctx context.Context, sessionID string, limit int) []chat.Message {
	msgs, err := h.messageService.GetRecentMessagesBySession(ctx, sessionID, limit)
	if err != nil || len(msgs) == 0 {
		return nil
	}
	out := make([]chat.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "user" || m.Role == "assistant" {
			out = append(out, chat.Message{Role: m.Role, Content: m.Content})
		}
	}
	return out
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

// buildNormalUserPrompt assembles the user-side context for a plain answer.
// searchBody (when present) carries the page's currently-applied filters so the
// model can answer questions like "what is this page currently querying?".
func buildNormalUserPrompt(message string, page *AIPageContext, searchBody json.RawMessage, lang string) string {
	ctxObj := map[string]interface{}{}
	if page != nil {
		ctxObj["page"] = page
	}
	if len(searchBody) > 0 {
		ctxObj["searchBody"] = searchBody
	}
	ctxJSON, _ := json.Marshal(ctxObj)

	var sb strings.Builder
	sb.WriteString("[用户提供的结构化上下文]\n")
	sb.WriteString(string(ctxJSON))
	sb.WriteString("\n\n")
	sb.WriteString(message)
	sb.WriteString("\n\n[回复语言：" + lang + "]")
	return sb.String()
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

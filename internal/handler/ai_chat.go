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

// defaultAIAgentID 是 WEKNORA_AI_AGENT_ID 未设置时使用的 OPLink 智能体 ID。
// 部署时可通过环境变量 WEKNORA_AI_AGENT_ID 覆盖。
const defaultAIAgentID = "6a188aae-b3fb-45c7-b59b-1d0a4620a113"

// pendingAction 记录已识别但尚未解决的搜索任务状态，以 actionId 为键。
// v1 版本仅在内存中维护，重启后未完成的任务将丢失。
type pendingAction struct {
	ConversationID string
	RequestID      string
	Message        string
	Page           *AIPageContext
}

// AIChatHandler 实现自定义 /api/v1/ai/chat 协议。
type AIChatHandler struct {
	sessionService     interfaces.SessionService
	messageService     interfaces.MessageService
	modelService       interfaces.ModelService
	customAgentService interfaces.CustomAgentService
	suggestionService  interfaces.MessageSuggestionService
	config             *config.Config

	mu      sync.Mutex
	actions map[string]*pendingAction
}

// NewAIChatHandler 创建 AI 聊天处理器。
func NewAIChatHandler(
	sessionService interfaces.SessionService,
	messageService interfaces.MessageService,
	modelService interfaces.ModelService,
	customAgentService interfaces.CustomAgentService,
	suggestionService interfaces.MessageSuggestionService,
	cfg *config.Config,
) *AIChatHandler {
	return &AIChatHandler{
		sessionService:     sessionService,
		messageService:     messageService,
		modelService:       modelService,
		customAgentService: customAgentService,
		suggestionService:  suggestionService,
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
	case AIRequestTypeStart:
		h.handleStart(ctx, c, &req, tenantID)
	case AIRequestTypeMessage:
		h.handleMessage(ctx, c, &req, agent, tenantID)
	case AIRequestTypeSearchContext:
		h.handleSearchContext(ctx, c, &req, agent)
	default:
		h.writeEvent(c, h.errorEvent(&req, "unknown type: "+req.Type))
	}
}

// resolveAgent 加载 OPLink 智能体，该智能体的系统提示词和模型驱动此端点。
// 智能体 ID 的解析顺序：config.yaml (ai_chat.agent_id) -> 环境变量 WEKNORA_AI_AGENT_ID -> 内置默认值。
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

// resolveIntentAgent 加载用于意图分类的意图分析智能体。
// 其 ID 来自 ai_chat.intent_agent_id（与主智能体使用同一张 custom_agents 表），
// 若未配置则回退到主智能体。
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

// resolveRecommendAgent 加载专用的推荐智能体（ai_chat.recommend_agent_id），
// 若未配置则回退到主智能体。
func (h *AIChatHandler) resolveRecommendAgent(ctx context.Context) (*types.CustomAgent, error) {
	if h.config != nil && h.config.AIChat != nil && h.config.AIChat.RecommendAgentID != "" {
		agent, err := h.customAgentService.GetAgentByID(ctx, h.config.AIChat.RecommendAgentID)
		if err != nil || agent == nil {
			return nil, fmt.Errorf("recommend agent %s not found", h.config.AIChat.RecommendAgentID)
		}
		return agent, nil
	}
	return h.resolveAgent(ctx)
}

// applyModelOverride 返回一份应用了 ai_chat.model_id 覆盖的智能体副本。
// 覆盖只作用于本次请求（不写回 custom_agents 表）；未配置覆盖值、或覆盖值与
// 智能体自身模型相同时，原样返回入参指针。
//
// 必须覆盖 Config.ModelID 而非只在 QARequest 上带 SummaryModelID：
// resolveChatModelID 会先校验智能体自身的 model_id 有效，智能体那个若失效
// 则在读到 SummaryModelID 之前就已报错。
func (h *AIChatHandler) applyModelOverride(agent *types.CustomAgent) *types.CustomAgent {
	if agent == nil {
		return nil
	}
	if h.config == nil || h.config.AIChat == nil {
		return agent
	}
	override := strings.TrimSpace(h.config.AIChat.ModelID)
	if override == "" || override == strings.TrimSpace(agent.Config.ModelID) {
		return agent
	}
	// Config 是值类型字段，浅拷贝即可获得独立的 ModelID；其中的切片仍与原对象
	// 共享，但此处只覆写一个字符串字段，不会写穿。
	clone := *agent
	clone.Config.ModelID = override
	return &clone
}

// handleStart 根据提供的数据上下文建立会话，并返回由专用推荐智能体
// （ai_chat.recommend_agent_id）基于会话基础数据生成的推荐候选问题。
func (h *AIChatHandler) handleStart(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	tenantID uint64,
) {
	session, err := h.getOrCreateSession(ctx, req.ConversationID, tenantID)
	if err != nil || session == nil {
		h.writeEvent(c, h.errorEvent(req, "failed to create session"))
		return
	}

	// 将数据作为本次会话的基础数据持久化。
	if payload, err := json.Marshal(map[string]interface{}{
		"page":         req.Page,
		"searchBody":   req.SearchBody,
		"searchFields": req.SearchFields,
	}); err == nil {
		_, _ = h.messageService.CreateMessage(ctx, &types.Message{
			SessionID:   session.ID,
			Role:        "start",
			Content:     string(payload),
			RequestID:   req.RequestID,
			IsCompleted: true,
			CreatedAt:   time.Now(),
		})
	}

	recommendAgent, err := h.resolveRecommendAgent(ctx)
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, err.Error()))
		return
	}

	questions, err := h.generateRecommendQuestions(ctx, recommendAgent, req.Page, req.SearchFields, req.SearchBody, "")
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, "failed to generate recommendations: "+err.Error()))
		return
	}
	h.writeEvent(c, AIEvent{
		Version:        req.Version,
		Type:           AIEventSuggestions,
		ConversationID: req.ConversationID,
		RequestID:      req.RequestID,
		Suggestions:    questions,
	})
}

// generateRecommendQuestions 调用推荐智能体，基于会话基础数据和（可选的）当前对话内容生成候选问题。
func (h *AIChatHandler) generateRecommendQuestions(
	ctx context.Context,
	recommendAgent *types.CustomAgent,
	page *AIPageContext,
	searchFields []AISearchField,
	searchBody json.RawMessage,
	conversation string,
) ([]AISuggestionItem, error) {
	modelID := recommendAgent.Config.ModelID
	if modelID == "" && h.config != nil && h.config.AIChat != nil && h.config.AIChat.ModelID != "" {
		modelID = h.config.AIChat.ModelID
	}
	chatModel, err := h.modelService.GetChatModel(ctx, modelID)
	if err != nil {
		return nil, err
	}

	user := "会话基础数据：\n" + formatStartData(page, searchFields, searchBody)
	if strings.TrimSpace(conversation) != "" {
		user += "\n\n当前对话内容：\n" + conversation
	}

	resp, err := chatModel.Chat(ctx, []chat.Message{
		{Role: "system", Content: recommendAgent.Config.SystemPrompt},
		{Role: "user", Content: user},
	}, &chat.ChatOptions{Temperature: 0.3})
	if err != nil {
		return nil, err
	}
	return parseRecommendQuestions(resp.Content)
}

// parseRecommendQuestions 从模型输出中提取 {"questions":[...]} 的 JSON。
func parseRecommendQuestions(content string) ([]AISuggestionItem, error) {
	content = strings.TrimSpace(content)
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("model returned invalid JSON")
	}
	var envelope struct {
		Questions []struct {
			Text string `json:"text"`
		} `json:"questions"`
	}
	if err := json.Unmarshal([]byte(content[start:end+1]), &envelope); err != nil {
		return nil, fmt.Errorf("decode questions JSON: %w", err)
	}
	items := make([]AISuggestionItem, 0, len(envelope.Questions))
	for _, q := range envelope.Questions {
		text := strings.TrimSpace(q.Text)
		if text == "" {
			continue
		}
		items = append(items, AISuggestionItem{ID: uuid.NewString(), Text: text})
	}
	return items, nil
}

// formatStartData 将 start 数据上下文渲染为可读的文本描述，
// 以便推荐生成器基于此生成问题。
func formatStartData(page *AIPageContext, searchFields []AISearchField, searchBody json.RawMessage) string {
	var sb strings.Builder
	if page != nil {
		sb.WriteString("页面类型：" + page.PageType)
		if page.ItemTypeName != "" {
			sb.WriteString("，对象：" + page.ItemTypeName)
		}
		sb.WriteString("\n")
	}
	if len(searchFields) > 0 {
		sb.WriteString("可搜索字段：")
		for i, f := range searchFields {
			if i > 0 {
				sb.WriteString("、")
			}
			sb.WriteString(f.Label)
			if f.Type == "select" && len(f.Options) > 0 {
				sb.WriteString("(")
				for j, o := range f.Options {
					if j > 0 {
						sb.WriteString("/")
					}
					sb.WriteString(o.Label)
				}
				sb.WriteString(")")
			}
		}
		sb.WriteString("\n")
	}
	if len(searchBody) > 0 {
		sb.WriteString("当前筛选：")
		sb.WriteString(string(searchBody))
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

// handleMessage 通过意图分析智能体对消息进行分类，持久化意图，
// 然后分发到搜索任务或智能体回答流程。
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
		h.handleSearchMessage(ctx, c, req, agent, tenantID)
		return
	}
	h.handleAgentTurn(ctx, c, req, agent, tenantID)
}

// classifyIntent 使用意图分析智能体的系统提示词 + 模型（来自 ai_chat.intent_agent_id）
// 返回 "search" 或 "normal"。若意图智能体未声明自身模型，则回退到 ai_chat.model_id，
// 再回退到主智能体的模型。
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

// persistIntent 将分类得到的意图作为专用的 "intent" 消息存入消息表。
// 历史构建器只会消费 user/assistant 角色，因此该消息可查询但不会污染 LLM 上下文。
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

// handleSearchMessage 处理搜索意图的消息。
// 若会话在 start 阶段已携带 searchFields，则直接生成条件，避免前端往返；
// 否则要求前端提供 searchFields 和 conditionRules。
func (h *AIChatHandler) handleSearchMessage(
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

	// 持久化用户消息，保证会话历史完整。
	_, _ = h.messageService.CreateMessage(ctx, &types.Message{
		SessionID:   session.ID,
		Role:        "user",
		Content:     req.Message,
		RequestID:   req.RequestID,
		IsCompleted: true,
		CreatedAt:   time.Now(),
	})

	// 若 start 阶段已提供 searchFields，则直接生成条件（conditionRules 包含在智能体系统提示词中）。
	page, searchFields, searchBody := h.loadStartContext(ctx, session.ID)
	if len(searchFields) > 0 {
		h.generateAndEmitSearchDraft(ctx, c, req, agent, page, searchFields, searchBody, nil)
		return
	}

	actionID := uuid.New().String()
	h.mu.Lock()
	h.actions[actionID] = &pendingAction{
		ConversationID: req.ConversationID,
		RequestID:      req.RequestID,
		Message:        req.Message,
		Page:           req.Page,
	}
	h.mu.Unlock()

	h.writeEvent(c, AIEvent{
		Version:        req.Version,
		Type:           AIEventNeedSearchContext,
		ConversationID: req.ConversationID,
		RequestID:      req.RequestID,
		ActionID:       actionID,
		Required:       []string{"searchFields", "conditionRules"},
	})
}

// handleSearchContext 是搜索流程的第二阶段（当 start 未提供 searchFields 时）：
// 生成条件 + searchBody，校验后返回 search_draft 或 need_clarification。
func (h *AIChatHandler) handleSearchContext(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	agent *types.CustomAgent,
) {
	h.mu.Lock()
	action := h.actions[req.ActionID]
	h.mu.Unlock()
	if action == nil {
		h.writeEvent(c, h.errorEvent(req, "action not found: "+req.ActionID))
		return
	}

	h.generateAndEmitSearchDraft(ctx, c, req, agent, action.Page, req.SearchFields, req.SearchBody, req.ConditionRules)
}

// generateAndEmitSearchDraft 根据给定的 searchFields（及可选的 conditionRules）生成搜索条件，
// 校验后发出 search_draft 或 need_clarification 事件。
func (h *AIChatHandler) generateAndEmitSearchDraft(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	agent *types.CustomAgent,
	page *AIPageContext,
	searchFields []AISearchField,
	searchBody json.RawMessage,
	rules *AIConditionRules,
) {
	modelID := agent.Config.ModelID
	if h.config != nil && h.config.AIChat != nil && h.config.AIChat.ModelID != "" {
		modelID = h.config.AIChat.ModelID
	}
	chatModel, err := h.modelService.GetChatModel(ctx, modelID)
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, "model unavailable: "+err.Error()))
		return
	}

	lang := detectReplyLanguage(req.Message)
	userContent := buildSearchPrompt(req.Message, page, searchFields, rules, lang)
	logger.Infof(ctx, "[ai-chat] search draft: agent=%s model=%s searchFields=%d sysPromptLen=%d",
		agent.ID, modelID, len(searchFields), len(agent.Config.SystemPrompt))
	resp, err := chatModel.Chat(ctx, []chat.Message{
		{Role: "system", Content: agent.Config.SystemPrompt},
		{Role: "user", Content: userContent},
	}, &chat.ChatOptions{Temperature: 0.1})
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, "model call failed: "+err.Error()))
		return
	}
	logger.Infof(ctx, "[ai-chat] search draft model output: %s", resp.Content)

	result, err := parseSearchResult(resp.Content)
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, "model output is not valid JSON: "+err.Error()))
		return
	}

	unmatched := validateSearchResult(result, searchFields, rules)
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

	// 同时基于会话数据和查询生成推荐问题。
	var suggestions []AISuggestionItem
	if recommendAgent, rerr := h.resolveRecommendAgent(ctx); rerr == nil {
		if s, serr := h.generateRecommendQuestions(ctx, recommendAgent, page, searchFields, searchBody, req.Message); serr == nil {
			suggestions = s
		}
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
		Suggestions:    suggestions,
	})
}

// loadStartContext 返回会话中最近一条 "start" 消息存储的 page/searchFields/searchBody，
// 以便搜索流程复用，无需再次与前端交互。
func (h *AIChatHandler) loadStartContext(
	ctx context.Context,
	sessionID string,
) (*AIPageContext, []AISearchField, json.RawMessage) {
	msgs, err := h.messageService.GetRecentMessagesBySession(ctx, sessionID, 50)
	if err != nil {
		return nil, nil, nil
	}
	// 消息按时间升序；从后往前扫描最新的 "start" 消息。
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m == nil || m.Role != "start" {
			continue
		}
		var payload struct {
			Page         *AIPageContext  `json:"page"`
			SearchBody   json.RawMessage `json:"searchBody"`
			SearchFields []AISearchField `json:"searchFields"`
		}
		if err := json.Unmarshal([]byte(m.Content), &payload); err != nil {
			continue
		}
		return payload.Page, payload.SearchFields, payload.SearchBody
	}
	return nil, nil, nil
}

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
	page, searchFields, searchBody := h.loadStartContext(ctx, session.ID)
	if req.Page == nil {
		req.Page = page
	}
	if len(req.SearchFields) == 0 {
		req.SearchFields = searchFields
	}
	if len(req.SearchBody) == 0 {
		req.SearchBody = searchBody
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
		_ = h.sessionService.AgentQA(ctx, qaReq, bus)
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
			if suggestions, serr := t.h.generateRecommendQuestions(ctx, recommendAgent, t.req.Page, t.req.SearchFields, t.req.SearchBody, conversation); serr == nil && len(suggestions) > 0 {
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

// getOrCreateSession 将不透明的 conversationId 映射到 WeKnora 会话，
// 首次使用时创建。会话所有者来自 SessionOwnerIDFromContext，
// 以确保与 CreateMessage 的会话查找范围一致。
// 若已存在的会话所有者不匹配（例如由早期错误合成用户导致），则就地修复，
// 使后续每轮对话的带范围查找都能成功。
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

// setSSEHeaders 设置流式响应头。
func (h *AIChatHandler) setSSEHeaders(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
}

// writeEvent 以 `data: {json}\n\n` 格式序列化一个 SSE 事件。
func (h *AIChatHandler) writeEvent(c *gin.Context, ev AIEvent) {
	b, _ := json.Marshal(ev)
	_, _ = c.Writer.WriteString("data: " + string(b) + "\n\n")
	c.Writer.Flush()
}

// errorEvent 构建一个终止性错误事件。
func (h *AIChatHandler) errorEvent(req *AIChatRequest, msg string) AIEvent {
	return AIEvent{
		Version:        req.Version,
		Type:           AIEventError,
		ConversationID: req.ConversationID,
		RequestID:      req.RequestID,
		Message:        msg,
	}
}

// buildSearchPrompt 组装条件生成所需的用户侧上下文。
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
	sb.WriteString("\n\n这是一次搜索任务。请把用户请求转换成搜索条件，只输出一个纯 JSON 对象（以 { 开头、以 } 结尾），禁止使用 markdown 代码块、禁止任何解释文字。")
	sb.WriteString("\n输出格式：{\"conditions\":[{\"field\":\"字段name\",\"label\":\"字段label\",\"type\":\"字段type\",\"operator\":\"操作符\",\"value\":值}]}。")
	sb.WriteString("其中 field/label/type 必须来自上面 searchFields；operator 按字段类型与系统提示词规则取值；select 字段的 value 必须是其 options 中的某个 value。")
	return sb.String()
}

// parseSearchResult 提取并解析模型输出的 JSON。
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

// extractJSONObject 去除 markdown 代码块标记，返回最外层的 JSON 对象。
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

// detectReplyLanguage 根据查询内容判断回复语言：繁体中文、简体中文或英文。
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

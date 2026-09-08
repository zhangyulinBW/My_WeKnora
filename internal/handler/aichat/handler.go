package aichat

// 本文件是 ai_chat 接口的协议入口层：定义 Handler、构造函数、请求分发入口
// （Chat），以及被多个子流程共享的基础能力——智能体解析、会话创建、SSE 输出。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// defaultAIAgentID 是 WEKNORA_AI_AGENT_ID 未设置时使用的 OPLink 智能体 ID。
// 部署时可通过环境变量 WEKNORA_AI_AGENT_ID 覆盖。
const defaultAIAgentID = "6a188aae-b3fb-45c7-b59b-1d0a4620a113"

// inlineAIAgentID 是 ai_chat.agent 内联定义未声明 id 时使用的合成智能体 ID，
// 会随助手消息落库（messages.agent_id）。该 ID 不存在于 custom_agents 表，
// 仅用于标识「本轮由 config.yaml 内联定义的智能体驱动」。
const inlineAIAgentID = "ai-chat-inline-agent"

// AIChatHandler 实现自定义 /api/v1/ai/chat 协议。
// 它是协议入口 + 智能体解析/会话/SSE 输出等基础设施；意图分类与分发见 intent.go，
// 搜索流程见 search.go，Agent 执行见 agent_turn.go，start 流程见 start.go。
type AIChatHandler struct {
	sessionService     interfaces.SessionService
	messageService     interfaces.MessageService
	modelService       interfaces.ModelService
	customAgentService interfaces.CustomAgentService
	suggestionService  interfaces.MessageSuggestionService
	config             *config.Config

	// intents 是意图注册表：意图分类结果 -> 对应处理逻辑（见 intent.go）。
	intents *intentRegistry

	// mu 保护 actions 的并发读写；actions 记录搜索任务待处理状态（见 search.go）。
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
	h := &AIChatHandler{
		sessionService:     sessionService,
		messageService:     messageService,
		modelService:       modelService,
		customAgentService: customAgentService,
		suggestionService:  suggestionService,
		config:             cfg,
		intents:            newIntentRegistry(),
		actions:            make(map[string]*pendingAction),
	}
	// 意图注册：normal 作为默认/回退意图最先注册，search 其次。
	h.intents.register(&normalIntentHandler{h: h})
	h.intents.register(&searchIntentHandler{h: h})
	return h
}

// Chat godoc
// @Summary      AI 自定义聊天
// @Description  自定义 AI 接口协议（OPLink），SSE 流式响应
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

	// 按请求 type 分发到协议阶段：start 建立会话上下文，message 进入意图分析 + 分发，
	// search_context 是搜索流程的第二阶段（前端回传搜索字段）。
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
// 解析顺序：config.yaml 内联定义 (ai_chat.agent) -> config.yaml (ai_chat.agent_id)
// -> 环境变量 WEKNORA_AI_AGENT_ID -> 内置默认值。内联定义存在时不查 custom_agents 表。
func (h *AIChatHandler) resolveAgent(ctx context.Context) (*types.CustomAgent, error) {
	if agent := h.agentFromInlineConfig(ctx); agent != nil {
		return agent, nil
	}
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

// agentFromInlineConfig 把 config.yaml 的 ai_chat.agent 内联定义构造成一次性的
// *types.CustomAgent（不落库）。未配置内联定义时返回 nil，由调用方回退到
// agent_id 解析链。构造方式对齐内置智能体的 buildAgentFromEntry：按请求 tenant
// 盖章、补默认值，从而正常走 AgentQA 的检索租户与模型解析。
func (h *AIChatHandler) agentFromInlineConfig(ctx context.Context) *types.CustomAgent {
	if h.config == nil || h.config.AIChat == nil || h.config.AIChat.Agent == nil {
		return nil
	}
	def := h.config.AIChat.Agent
	id := strings.TrimSpace(def.ID)
	if id == "" {
		id = inlineAIAgentID
	}
	tenantID, _ := types.TenantIDFromContext(ctx)
	agent := &types.CustomAgent{
		ID:          id,
		Name:        def.Name,
		Description: def.Description,
		TenantID:    tenantID,
		Config:      def.CustomAgentConfig,
	}
	agent.EnsureDefaults()
	return agent
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

// resolveSearchAgent 加载用于搜索条件生成的智能体。
// 其 ID 来自 ai_chat.search_agent_id（其 system_prompt 定义搜索规则），
// 若未配置则回退到主智能体。
func (h *AIChatHandler) resolveSearchAgent(ctx context.Context) (*types.CustomAgent, error) {
	if h.config != nil && h.config.AIChat != nil && h.config.AIChat.SearchAgentID != "" {
		agent, err := h.customAgentService.GetAgentByID(ctx, h.config.AIChat.SearchAgentID)
		if err != nil || agent == nil {
			return nil, fmt.Errorf("search agent %s not found", h.config.AIChat.SearchAgentID)
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
	// Be explicit about the SSE event name. It is semantically equivalent to
	// omitting it ("message" is the SSE default), but makes this endpoint's
	// frames match the standard chat stream for raw SSE consumers.
	_, _ = c.Writer.WriteString("event: message\n" + "data: " + string(b) + "\n\n")
	c.Writer.Flush()
}

// errorEvent 构建一个终止性错误事件。
func (h *AIChatHandler) errorEvent(req *AIChatRequest, msg string) AIEvent {
	return AIEvent{
		ID:             req.RequestID,
		ResponseType:   AIEventError,
		Version:        req.Version,
		Type:           AIEventError,
		ConversationID: req.ConversationID,
		RequestID:      req.RequestID,
		Content:        msg,
		Message:        msg,
		Done:           true,
		Data:           map[string]interface{}{"error": msg},
	}
}

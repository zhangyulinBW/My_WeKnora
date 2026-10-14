package aichat

// 本文件实现 ai_chat 的意图分析与分发：把用户消息分类为类型化 Intent，
// 再经意图注册表分发到对应处理逻辑（search 结构化搜索 / approval 签审草案 /
// normal 智能体问答）。搜索与签核意图是否走结构化流程分别由 shouldSearch、
// shouldApprove 控制（config 开关 + 页面类型）。

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// Intent 是 ai_chat 协议内的类型化意图标识。
// 其字符串值与历史已落库的 intent 消息保持一致（"search"/"normal"），
// 保证新增意图不会破坏已有会话里已持久化的意图记录。
type Intent string

// 已识别的意图。normal 是默认/回退意图，search 是搜索任务意图，
// approval 是流程签审草案意图（仅 processApproval 页面生效）。
const (
	IntentNormal   Intent = "normal"
	IntentSearch   Intent = "search"
	IntentApproval Intent = "approval"
)

// intentHandler 是某个意图的处理逻辑。
// 新增意图 = 新增一个 Intent 常量 + 一个实现并注册。
type intentHandler interface {
	// Intent 返回该 handler 负责的意图标识（即注册表的分发键）。
	Intent() Intent
	// Handle 处理该意图的一轮请求。参数与 handleMessage 分发入口保持一致。
	Handle(ctx context.Context, c *gin.Context, req *AIChatRequest, agent *types.CustomAgent, tenantID uint64)
}

// intentRegistry 按 Intent 分发到对应的 intentHandler；未注册的意图由调用方兜底到 normal。
type intentRegistry struct {
	handlers map[Intent]intentHandler
}

// newIntentRegistry 创建空注册表。
func newIntentRegistry() *intentRegistry {
	return &intentRegistry{handlers: make(map[Intent]intentHandler)}
}

// register 将一个意图 handler 注册进注册表，以 handler.Intent() 为键。
func (r *intentRegistry) register(h intentHandler) {
	r.handlers[h.Intent()] = h
}

// resolve 返回指定意图对应的 handler，未命中返回 nil（由调用方兜底 normal）。
func (r *intentRegistry) resolve(intent Intent) intentHandler {
	return r.handlers[intent]
}

// searchIntentHandler 是搜索意图的适配器，委托给 AIChatHandler.handleSearchMessage。
type searchIntentHandler struct{ h *AIChatHandler }

func (s *searchIntentHandler) Intent() Intent { return IntentSearch }

func (s *searchIntentHandler) Handle(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	agent *types.CustomAgent,
	tenantID uint64,
) {
	s.h.handleSearchMessage(ctx, c, req, agent, tenantID)
}

// normalIntentHandler 是默认意图（普通问答）的适配器，委托给 AIChatHandler.handleAgentTurn。
type normalIntentHandler struct{ h *AIChatHandler }

func (n *normalIntentHandler) Intent() Intent { return IntentNormal }

func (n *normalIntentHandler) Handle(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	agent *types.CustomAgent,
	tenantID uint64,
) {
	n.h.handleAgentTurn(ctx, c, req, agent, tenantID)
}

// approvalIntentHandler 是签核意图的适配器，委托给 AIChatHandler.handleApprovalMessage。
type approvalIntentHandler struct{ h *AIChatHandler }

func (a *approvalIntentHandler) Intent() Intent { return IntentApproval }

func (a *approvalIntentHandler) Handle(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	agent *types.CustomAgent,
	tenantID uint64,
) {
	a.h.handleApprovalMessage(ctx, c, req, agent, tenantID)
}

// handleMessage 通过意图分析智能体对消息进行分类、持久化意图，
// 然后经意图注册表分发到对应的处理逻辑。
func (h *AIChatHandler) handleMessage(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	agent *types.CustomAgent,
	tenantID uint64,
) {
	// 消息请求可能省略 page（协议允许省略未变化字段），而意图分类与
	// search/approval 门控都依赖页面类型：回退最近一次 start 同步的页面，
	// 保证 processApproval 会话的 approval 判定不因省略 page 而失效。
	if req.Page == nil {
		if session, err := h.getOrCreateSession(ctx, req.ConversationID, tenantID); err == nil && session != nil {
			if p, _, _, _ := h.loadStartContext(ctx, session.ID); p != nil {
				req.Page = p
			}
		}
	}

	// 携带附件/图片的消息直接走 normal：结构化搜索面向表格筛选，附件内容
	// 对条件生成没有意义；且「仅附件无文本」的消息也无法做意图分类。
	intent := IntentNormal
	if !req.hasUploads {
		var err error
		intent, err = h.classifyIntent(ctx, req.Message, req.Page, agent)
		if err != nil {
			logger.Warnf(ctx, "AI chat intent classification failed, defaulting to normal: %v", err)
			intent = IntentNormal
		}
	}

	// 提示词契约里「统计/图表/汇总」等分析类请求归 normal（产物是统计结果或
	// 图表，而非筛选后的记录列表），但意图模型对这类表述仍有概率误判为
	// search（同一句话多次分类结果不一致）。命中分析特征词时按确定性规则
	// 纠正，保证路由稳定。
	if intent == IntentSearch && looksLikeAnalysisRequest(req.Message) {
		logger.Infof(ctx, "AI chat intent corrected search->normal (analysis guard): %s", req.Message)
		intent = IntentNormal
	}

	// 同一分析类误判对 approval 同样存在：processApproval 页面上的统计请求
	// （如「统计任务数量」）会被页面语境带偏成 approval。签核动作类表述
	// （同意/提交/勾选清单等）不含分析特征词，不受该守卫影响。
	if intent == IntentApproval && looksLikeAnalysisRequest(req.Message) {
		logger.Infof(ctx, "AI chat intent corrected approval->normal (analysis guard): %s", req.Message)
		intent = IntentNormal
	}

	// 搜索开关：search 意图只在「开关开启 + 页面为 largeTable」时才走结构化搜索流程；
	// 否则回退 normal（singleData 等场景答案就在 itemdata 里，直接让 Agent 流式回答）。
	if intent == IntentSearch && !h.shouldSearch(ctx, req, tenantID) {
		intent = IntentNormal
	}

	// 签核开关：approval 意图只在「开关开启 + 页面为 processApproval」时才走
	// 签审草案流程；否则回退 normal（其他页面的误分类不产生签核草案）。
	if intent == IntentApproval && !h.shouldApprove(ctx, req, tenantID) {
		intent = IntentNormal
	}

	h.persistIntent(ctx, req, tenantID, intent)

	// 意图注册表分发：新增意图无需改这里。
	handler := h.intents.resolve(intent)
	if handler == nil {
		// 未知意图兜底走智能体问答。
		handler = &normalIntentHandler{h: h}
	}
	handler.Handle(ctx, c, req, agent, tenantID)
}

// shouldSearch 判断当前请求是否应走结构化搜索流程：
// 仅当 config 开启搜索开关、且页面类型为 largeTable 时返回 true。
// 页面优先取当前请求的 page，未携带时回退到 start 上下文里的 page。
func (h *AIChatHandler) shouldSearch(ctx context.Context, req *AIChatRequest, tenantID uint64) bool {
	if h.config == nil || h.config.AIChat == nil || !h.config.AIChat.SearchEnabled {
		return false
	}
	page := req.Page
	if page == nil || page.PageType == "" {
		if session, err := h.getOrCreateSession(ctx, req.ConversationID, tenantID); err == nil && session != nil {
			if p, _, _, _ := h.loadStartContext(ctx, session.ID); p != nil {
				page = p
			}
		}
	}
	return page != nil && page.PageType == "largeTable"
}

// shouldApprove 判断当前请求是否应走签审草案流程：
// 仅当 config 开启签核开关、且页面类型为 processApproval 时返回 true。
// 页面优先取当前请求的 page，未携带时回退到 start 上下文里的 page。
func (h *AIChatHandler) shouldApprove(ctx context.Context, req *AIChatRequest, tenantID uint64) bool {
	if h.config == nil || h.config.AIChat == nil || !h.config.AIChat.ApprovalEnabled {
		return false
	}
	page := req.Page
	if page == nil || page.PageType == "" {
		if session, err := h.getOrCreateSession(ctx, req.ConversationID, tenantID); err == nil && session != nil {
			if p, _, _, _ := h.loadStartContext(ctx, session.ID); p != nil {
				page = p
			}
		}
	}
	return page != nil && page.PageType == PageTypeProcessApproval
}

// classifyIntent 使用意图分析智能体的系统提示词 + 模型（来自 ai_chat.intent_agent_id）
// 返回类型化的 Intent。若意图智能体未声明自身模型，则回退到 ai_chat.model_id，
// 再回退到主智能体的模型。
//
// 模型输出被归一化后与已知意图比对：命中则返回对应 Intent，未命中回退 IntentNormal。
func (h *AIChatHandler) classifyIntent(
	ctx context.Context,
	message string,
	page *AIPageContext,
	mainAgent *types.CustomAgent,
) (Intent, error) {
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

	// 归一化模型输出，映射到已知意图；无法识别时回退 normal。
	key := strings.ToLower(strings.TrimSpace(resp.Content))
	switch key {
	case string(IntentSearch):
		return IntentSearch, nil
	case string(IntentApproval):
		return IntentApproval, nil
	default:
		return IntentNormal, nil
	}
}

// analysisKeywords 是分析/图表类请求的特征词，与意图提示词中归 normal 的
// 「数据分析与生成类任务」条款保持一致。列表刻意保守：只收无歧义的分析
// 词汇，避免把真正的筛选指令（如「筛选金额大于10万的单」）误纠正为 normal。
var analysisKeywords = []string{
	"统计", "汇总", "聚合", "计数", "求和", "平均值",
	"图表", "柱状", "饼图", "折线", "可视化", "报表",
	"画个", "画一", "生成图", "分布", "占比",
}

// looksLikeAnalysisRequest 判断消息是否为分析/图表类请求（按契约应走 normal
// 流）。仅在意图模型判为 search 或 approval 时用于兜底纠正，normal 判定不受
// 影响。
func looksLikeAnalysisRequest(message string) bool {
	for _, kw := range analysisKeywords {
		if strings.Contains(message, kw) {
			return true
		}
	}
	return false
}

// persistIntent 将分类得到的意图作为专用的 "intent" 消息存入消息表。
// 历史构建器只会消费 user/assistant 角色，因此该消息可查询但不会污染 LLM 上下文。
func (h *AIChatHandler) persistIntent(ctx context.Context, req *AIChatRequest, tenantID uint64, intent Intent) {
	session, err := h.getOrCreateSession(ctx, req.ConversationID, tenantID)
	if err != nil || session == nil {
		return
	}
	content, _ := json.Marshal(map[string]string{"intent": string(intent)})
	_, _ = h.messageService.CreateMessage(ctx, &types.Message{
		SessionID:   session.ID,
		Role:        "intent",
		Content:     string(content),
		RequestID:   req.RequestID,
		IsCompleted: true,
		CreatedAt:   time.Now(),
	})
}

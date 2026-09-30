package aichat

// 本文件实现搜索意图的两阶段流程：识别搜索任务 -> 若缺少搜索字段则向前端索取，
// 否则生成搜索条件草稿、硬校验后发出 search_draft 或 need_clarification 事件。
// 该流程只在「搜索开关开启 + 页面为 largeTable」时被触发（见 intent.go 的 shouldSearch）。
// 条件生成与 normal 问答一致走 AgentQA（见 search_agent.go），search agent 关联的
// 知识库可通过 knowledge_search 工具参与字段判断。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// pendingAction 记录已识别但尚未解决的搜索任务状态，以 actionId 为键。
// v1 版本仅在内存中维护，重启后未完成的任务将丢失。
type pendingAction struct {
	ConversationID string
	RequestID      string
	Message        string
	Page           *AIPageContext
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

	// 若 start 阶段已提供 searchFields，则直接生成条件（conditionRules 包含在智能体上下文中）。
	page, searchFields, searchBody, _ := h.loadStartContext(ctx, session.ID)
	if len(searchFields) > 0 {
		h.generateAndEmitSearchDraft(ctx, c, req, session, page, searchFields, searchBody, nil)
		return
	}

	// 缺少搜索字段：生成 actionID 存下待处理状态，通知前端补齐 searchFields/conditionRules。
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
	tenantID uint64,
) {
	h.mu.Lock()
	action := h.actions[req.ActionID]
	h.mu.Unlock()
	if action == nil {
		h.writeEvent(c, h.errorEvent(req, "action not found: "+req.ActionID))
		return
	}

	// 第二阶段请求由前端补齐字段触发，可不回传原始用户消息与页面上下文；
	// 回填第一阶段记录的值，保证条件生成仍面向用户的原始请求。
	if strings.TrimSpace(req.Message) == "" {
		req.Message = action.Message
	}
	if req.Page == nil {
		req.Page = action.Page
	}
	session, err := h.getOrCreateSession(ctx, action.ConversationID, tenantID)
	if err != nil || session == nil {
		h.writeEvent(c, h.errorEvent(req, "failed to create session"))
		return
	}

	h.generateAndEmitSearchDraft(ctx, c, req, session, action.Page, req.SearchFields, req.SearchBody, req.ConditionRules)
}

// generateAndEmitSearchDraft 根据给定的 searchFields（及可选的 conditionRules），
// 以 AgentQA 方式驱动 search agent 生成搜索条件（进度事件流式输出），校验后
// 发出 search_draft 或 need_clarification 事件。与 normal 流（handleAgentTurn）
// 共用同一引擎路径：search agent 关联的知识库通过 knowledge_search 工具参与
// 字段判断，系统提示词占位符（{{current_time}} 等）由引擎侧统一渲染。
func (h *AIChatHandler) generateAndEmitSearchDraft(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	session *types.Session,
	page *AIPageContext,
	searchFields []AISearchField,
	searchBody json.RawMessage,
	rules *AIConditionRules,
) {
	// 搜索条件生成使用专门的 search agent（其 system_prompt 定义搜索规则），
	// 若未配置 ai_chat.search_agent_id 则回退到主智能体。
	searchAgent, err := h.resolveSearchAgent(ctx)
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, err.Error()))
		return
	}
	// 与 normal 流不同，这里优先使用 search agent 自己配置的模型：
	// search agent 是专门为条件生成调校的（模型/温度/预算），ai_chat.model_id
	// 的全局覆盖只在该智能体未声明模型时兜底（经 QARequest.SummaryModelID）。
	qaModelID := ""
	if strings.TrimSpace(searchAgent.Config.ModelID) == "" &&
		h.config != nil && h.config.AIChat != nil {
		qaModelID = h.config.AIChat.ModelID
	}

	// 预创建助手消息：智能体步骤与最终草稿 JSON 落库，后续轮次的 refinement
	// （如「改成去年」）能基于历史条件继续。
	assistantMsg, err := h.messageService.CreateMessage(ctx, &types.Message{
		SessionID:     session.ID,
		Role:          "assistant",
		RequestID:     req.RequestID,
		IsCompleted:   false,
		CreatedAt:     time.Now(),
		AgentID:       searchAgent.ID,
		AgentTenantID: searchAgent.TenantID,
		ModelID:       searchAgent.Config.ModelID,
	})
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, "failed to persist assistant message: "+err.Error()))
		return
	}

	// Query 保持用户原始消息（供语言检测与检索工具构造查询）；结构化上下文
	// 与任务指令经 Metadata 注入，由 AgentQA 追加到智能体查询之后。
	qaReq := &types.QARequest{
		Session:            session,
		Query:              req.Message,
		AssistantMessageID: assistantMsg.ID,
		CustomAgent:        searchAgent,
		SummaryModelID:     qaModelID,
		WebSearchEnabled:   searchAgent.Config.WebSearchEnabled,
		Metadata:           buildSearchMetadata(page, searchFields, searchBody, rules),
	}
	logger.Infof(ctx, "[ai-chat] search draft via AgentQA: agent=%s model=%s searchFields=%d",
		searchAgent.ID, searchAgent.Config.ModelID, len(searchFields))

	bus := event.NewEventBus()
	done := make(chan struct{})
	base := newAIStreamTranslator(h, c, req, assistantMsg, session.TenantID, done)
	translator := newSearchDraftTranslator(base, &searchDraftDeps{
		h:            h,
		agent:        searchAgent,
		message:      req.Message,
		page:         page,
		searchFields: searchFields,
		searchBody:   searchBody,
		rules:        rules,
		lang:         detectReplyLanguage(req.Message),
	})
	translator.subscribe(bus)

	go func() {
		defer translator.close()
		// 多数运行期失败由 AgentQA 发到事件总线；部分装配失败（如开启
		// knowledge_search 却未配置 rerank 模型）在引擎创建前直接返回，
		// 需在此补发终止错误帧。
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

// loadStartContext 返回会话中最近一条 "start" 消息存储的 page/searchFields/searchBody/itemData，
// 以便搜索流程复用，无需再次与前端交互。
func (h *AIChatHandler) loadStartContext(
	ctx context.Context,
	sessionID string,
) (*AIPageContext, []AISearchField, json.RawMessage, json.RawMessage) {
	msgs, err := h.messageService.GetRecentMessagesBySession(ctx, sessionID, 50)
	if err != nil {
		return nil, nil, nil, nil
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
			ItemData     json.RawMessage `json:"itemdata"`
		}
		if err := json.Unmarshal([]byte(m.Content), &payload); err != nil {
			continue
		}
		// 历史 start 消息可能只带 data_type（旧前端），回读时同样归一化进 Type。
		normalizeSearchFields(payload.SearchFields)
		return payload.Page, payload.SearchFields, payload.SearchBody, payload.ItemData
	}
	return nil, nil, nil, nil
}

// buildSearchPrompt 组装纠正重试（searchDraftDeps.repair）所需的用户侧上下文。
// 任务契约（输出格式、字段与操作符规则等）完全由 search agent 的系统提示词
// 定义，这里只提供结构化上下文与用户请求原文，不重复任何契约条款。
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
	// 与 buildSearchMetadata 一致：无 conditionRules 时用当前日期合成 dateContext，
	// 保证纠正重试的输出与主路径的日期基准相同。
	if rules != nil && rules.DateContext.CurrentDate != "" {
		ctxObj["dateContext"] = rules.DateContext
	} else {
		ctxObj["dateContext"] = AIDateContext{
			CurrentDate: time.Now().Format("2006-01-02"),
			Timezone:    time.Local.String(),
			ValueFormat: "YYYY-MM-DD",
		}
	}
	ctxJSON, _ := json.Marshal(ctxObj)

	var sb strings.Builder
	sb.WriteString("[用户提供的结构化上下文]\n")
	sb.WriteString(string(ctxJSON))
	sb.WriteString("\n\n")
	sb.WriteString("用户请求：" + message)
	sb.WriteString("\n\n[回复语言：" + lang + "]")
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

package aichat

// 本文件实现搜索意图的两阶段流程：识别搜索任务 -> 若缺少搜索字段则向前端索取，
// 否则生成搜索条件草稿、硬校验后发出 search_draft 或 need_clarification 事件。
// 该流程只在「搜索开关开启 + 页面为 largeTable」时被触发（见 intent.go 的 shouldSearch）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
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

	// 若 start 阶段已提供 searchFields，则直接生成条件（conditionRules 包含在智能体系统提示词中）。
	page, searchFields, searchBody, _ := h.loadStartContext(ctx, session.ID)
	if len(searchFields) > 0 {
		h.generateAndEmitSearchDraft(ctx, c, req, agent, page, searchFields, searchBody, nil)
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
	// 搜索条件生成使用专门的 search agent（其 system_prompt 定义搜索规则），
	// 若未配置 ai_chat.search_agent_id 则回退到主智能体。
	searchAgent, err := h.resolveSearchAgent(ctx)
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, err.Error()))
		return
	}
	agent = searchAgent

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

	// 硬校验：字段/操作符/枚举值不合法则要求澄清，而不是直接返回不可用的草稿。
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
		if s, serr := h.generateRecommendQuestions(ctx, recommendAgent, page, searchFields, searchBody, req.ItemData, req.Message); serr == nil {
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
		return payload.Page, payload.SearchFields, payload.SearchBody, payload.ItemData
	}
	return nil, nil, nil, nil
}

// buildSearchPrompt 组装条件生成所需的用户侧上下文。
// 搜索规则（字段类型→操作符、操作符语义、输出格式等）由专门的 search agent
// 的 system_prompt 定义（见 resolveSearchAgent），这里只提供上下文与任务标记。
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

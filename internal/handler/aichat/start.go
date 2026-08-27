package aichat

// 本文件实现 start 协议阶段：根据前端提供的数据上下文建立会话，持久化基础数据，
// 并由专用推荐智能体生成候选推荐问题返回给前端。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

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
	// singleData 场景下 itemdata 是核心载荷，必须一并落库，否则后续轮次会丢失。
	if payload, err := json.Marshal(map[string]interface{}{
		"page":         req.Page,
		"searchBody":   req.SearchBody,
		"searchFields": req.SearchFields,
		"itemdata":     req.ItemData,
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

	questions, err := h.generateRecommendQuestions(ctx, recommendAgent, req.Page, req.SearchFields, req.SearchBody, req.ItemData, "")
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
	itemData json.RawMessage,
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

	user := "会话基础数据：\n" + formatStartData(page, searchFields, searchBody, itemData)
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
func formatStartData(page *AIPageContext, searchFields []AISearchField, searchBody json.RawMessage, itemData json.RawMessage) string {
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
	// singleData 场景：直接给出这条记录的原始 JSON，供推荐模型基于数据生成问题。
	if len(itemData) > 0 {
		sb.WriteString("单条数据：")
		sb.WriteString(string(itemData))
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

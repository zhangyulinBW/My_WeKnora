package aichat

// 本文件实现 skill 意图（技能执行）的处理：把用户消息送入专用的技能智能体
// （AgentQA），该智能体调用沙箱内的技能（如 xlsx-to-static-page 把 Excel 转成
// 静态 HTML 网页）。与 normal 流一样，思考/工具调用/工具结果等进度事件与最终
// 正文以 answer_chunk 实时流式输出；区别在完成路径：回收沙箱产物后，把 HTML
// 产物内联进 answer_done 的 previews，并在正文末尾注入「查看解析结果」锚点
// 链接（[查看解析结果](#ai-preview-file-1)），前端按锚点渲染对应 preview。

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// AIErrorCodeSkillTurn 是技能执行失败的错误类别标识（error 事件 code）。
const AIErrorCodeSkillTurn = "SKILL_TURN_ERROR"

// skillPreviewMaxBytes 是单个 preview 内联内容的上限。xlsx-to-static-page 产出的
// HTML 约 0.9–1 MB，2 MiB 上限留足余量；超限产物跳过内联（仍可经 artifacts 事件
// 下载），避免把超大二进制塞进 SSE 事件。
const skillPreviewMaxBytes int64 = 2 << 20 // 2 MiB

// handleSkillTurn 处理技能执行意图：解析专用技能智能体，经 AgentQA 驱动技能在
// 沙箱内执行（思考/工具进度实时流式输出），完成时回收产物、内联 HTML 预览并
// 发出携带 previews 的 answer_done 终止事件。
func (h *AIChatHandler) handleSkillTurn(
	ctx context.Context,
	c *gin.Context,
	req *AIChatRequest,
	agent *types.CustomAgent,
	tenantID uint64,
) {
	// 主智能体参数仅保持 intentHandler 接口形状；技能轮始终使用专用 skill
	// 智能体（skill 内联定义 / skill_agent_id → 主智能体）。
	skillAgent, err := h.resolveSkillAgent(ctx)
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, err.Error(), AIErrorCodeSkillTurn))
		return
	}
	skillAgent = h.applyModelOverride(ctx, req, skillAgent)

	session, err := h.getOrCreateSession(ctx, req.ConversationID, tenantID)
	if err != nil || session == nil {
		h.writeEvent(c, h.errorEvent(req, "failed to create session", AIErrorCodeSkillTurn))
		return
	}

	// 回填 start 阶段持久化的基础上下文，保证后续轮次仍能获得会话基础数据。
	page, _, _, itemData, additionalItems := h.loadStartContext(ctx, session.ID)
	if req.Page == nil {
		req.Page = page
	}
	if len(req.ItemData) == 0 {
		req.ItemData = itemData
	}
	if len(req.AdditionalItems) == 0 {
		req.AdditionalItems = additionalItems
	}

	// 持久化用户消息（附件随消息落库；AgentQA 会把附件预置进会话沙箱的
	// /workspace/input，供技能脚本读取）。
	if _, err := h.messageService.CreateMessage(ctx, &types.Message{
		SessionID:   session.ID,
		Role:        "user",
		Content:     req.Message,
		RequestID:   req.RequestID,
		IsCompleted: true,
		CreatedAt:   time.Now(),
		Images:      req.processedImages,
		Attachments: req.processedAttachments,
	}); err != nil {
		h.writeEvent(c, h.errorEvent(req, "failed to persist user message: "+err.Error(), AIErrorCodeSkillTurn))
		return
	}

	assistantMsg, err := h.messageService.CreateMessage(ctx, &types.Message{
		SessionID:     session.ID,
		Role:          "assistant",
		RequestID:     req.RequestID,
		IsCompleted:   false,
		CreatedAt:     time.Now(),
		AgentID:       skillAgent.ID,
		AgentTenantID: skillAgent.TenantID,
		ModelID:       skillAgent.Config.ModelID,
	})
	if err != nil {
		h.writeEvent(c, h.errorEvent(req, "failed to persist assistant message: "+err.Error(), AIErrorCodeSkillTurn))
		return
	}

	// 仅附件无文本时替用户补默认提问（用户消息仍落库原文）。
	query := req.Message
	if strings.TrimSpace(query) == "" && req.hasUploads {
		query = types.UploadOnlyQuestion(types.LanguageFromContextOrDefault(ctx))
	}

	qaReq := &types.QARequest{
		Session:            session,
		Query:              query,
		AssistantMessageID: assistantMsg.ID,
		CustomAgent:        skillAgent,
		WebSearchEnabled:   skillAgent.Config.WebSearchEnabled,
		Attachments:        req.processedAttachments,
		ImageURLs:          req.imageURLs,
		ImageDescription:   req.imageDescription,
		Metadata:           buildAIMetadata(req),
	}
	logger.Infof(ctx, "[ai-chat] skill turn via AgentQA: agent=%s model=%s", skillAgent.ID, skillAgent.Config.ModelID)

	bus := event.NewEventBus()
	done := make(chan struct{})
	base := newAIStreamTranslator(h, c, req, assistantMsg, session.TenantID, done)
	translator := newSkillStreamTranslator(base)
	translator.subscribe(bus)

	go func() {
		defer translator.close()
		if err := h.sessionService.AgentQA(ctx, qaReq, bus); err != nil {
			t := translator
			if t.c.Request.Context().Err() == nil {
				t.writeMu.Lock()
				t.h.writeEvent(t.c, t.h.errorEvent(t.req, err.Error(), AIErrorCodeSkillTurn))
				t.writeMu.Unlock()
			}
		}
	}()

	select {
	case <-done:
	case <-c.Request.Context().Done():
	}
}

// skillStreamTranslator 订阅技能智能体的 EventBus：进度事件（思考/工具调用/
// 工具结果/引用）复用 normal 流的 SSE 映射；最终答案实时流式为 answer_chunk，
// 完成时回收产物、内联 HTML 预览，并发出携带 previews 的 answer_done。
type skillStreamTranslator struct {
	*aiStreamTranslator
}

func newSkillStreamTranslator(base *aiStreamTranslator) *skillStreamTranslator {
	return &skillStreamTranslator{aiStreamTranslator: base}
}

// subscribe 注册技能完成路径的事件映射。
func (t *skillStreamTranslator) subscribe(bus *event.EventBus) {
	t.subscribeProgress(bus)

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
		// 正文实时流式输出；answer_done 推迟到完成事件（需先回收产物构造
		// previews，才能决定是否携带内联预览与补发预览链接）。
		if data.Content != "" {
			t.write(AIEventAnswerChunk, data.Content, false, nil)
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

		// 未流式回答但 complete 携带 final answer 时补发一次。
		if !answered && data.FinalAnswer != "" {
			t.write(AIEventAnswerChunk, data.FinalAnswer, false, nil)
			final = data.FinalAnswer
		}

		// 回收沙箱产物（重写 sandbox: 引用、附到助手消息），不发事件。
		attached, final, hasArtifacts := t.collectAndRewriteArtifacts(ctx, final)

		// 内联 HTML 产物为 previews，并在正文末尾注入锚点链接。
		previews := buildSkillPreviews(ctx, t.h, attached)
		if len(previews) > 0 {
			link := skillPreviewLink(previews)
			// 模型可能已按提示词输出链接；仅在缺失时由后端确定性补发，
			// 避免重复。
			if !strings.Contains(final, "#ai-preview-file-") {
				final += "\n\n" + link
				t.write(AIEventAnswerChunk, "\n\n"+link, false, nil)
			}
		}

		// 智能体模式下知识引用经 AgentCompleteData.KnowledgeRefs 提供。
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

		// 落库助手消息：正文含注入的预览链接，产物列表已附到消息。
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

		// 终止事件：answer_done 携带内联 previews（无产物时 previews 为空）。
		t.writeSkillAnswerDone(previews)

		// 产物下载事件（如有）：previews 负责内联查看，artifacts 提供下载路径，
		// 与 normal 流的产物协议保持一致。
		if hasArtifacts {
			t.writeArtifacts(attached, final)
		}

		// 回答完成后触发推荐问题。
		if t.h.suggestionService != nil {
			bgCtx := context.WithoutCancel(ctx)
			go func() {
				if _, err := t.h.suggestionService.EnsureFollowUps(bgCtx, t.asstMsg.SessionID, t.asstMsg.ID, false); err != nil {
					logger.Warnf(bgCtx, "follow-up suggestion generation failed for message %s: %v", t.asstMsg.ID, err)
				}
			}()
		}
		if recommendAgent, rerr := t.h.resolveRecommendAgent(ctx); rerr == nil {
			conversation := t.req.Message
			if final != "" {
				conversation += "\n\n" + final
			}
			suggestions, serr := t.h.generateRecommendQuestions(
				ctx, recommendAgent, t.req.Page, t.req.SearchFields, t.req.SearchBody,
				t.req.ItemData, t.req.AdditionalItems, conversation,
			)
			if serr == nil && len(suggestions) > 0 {
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
		t.writeMu.Lock()
		t.h.writeEvent(t.c, t.h.errorEvent(t.req, data.Error, AIErrorCodeSkillTurn))
		t.writeMu.Unlock()
		t.close()
		return nil
	})
}

// writeSkillAnswerDone 发出技能意图的终止 answer_done 事件：data 与 normal 流
// 一致（session_id + assistant_message_id），顶层额外携带 previews。
func (t *skillStreamTranslator) writeSkillAnswerDone(previews []AIPreview) {
	if t.c.Request.Context().Err() != nil {
		return
	}
	t.writeMu.Lock()
	t.h.writeEvent(t.c, AIEvent{
		Version:        t.req.Version,
		Type:           AIEventAnswerDone,
		ConversationID: t.req.ConversationID,
		RequestID:      t.req.RequestID,
		Done:           true,
		Data: map[string]interface{}{
			"session_id":           t.asstMsg.SessionID,
			"assistant_message_id": t.asstMsg.ID,
		},
		Previews: previews,
	})
	t.writeMu.Unlock()
}

// buildSkillPreviews 从本轮产物中挑出 HTML 文件，读回完整内容内联为 AIPreview。
// 产物编号按 HTML 文件出现顺序 file-1、file-2…，与正文锚点 #ai-preview-<id>
// 一一对应。非 HTML、已删除、读失败或超限的产物跳过（仍可经 artifacts 下载）。
func buildSkillPreviews(ctx context.Context, h *AIChatHandler, artifacts types.MessageArtifacts) []AIPreview {
	if len(artifacts) == 0 || h == nil || h.fileService == nil {
		return nil
	}
	var previews []AIPreview
	for _, a := range artifacts {
		if a.Deleted() || a.URL == "" {
			continue
		}
		if a.FileType != ".html" && a.FileType != ".htm" {
			continue
		}
		if skillPreviewMaxBytes > 0 && a.FileSize > skillPreviewMaxBytes {
			logger.Warnf(ctx, "[ai-chat] skill preview skipped (oversize) file=%s size=%d", a.FileName, a.FileSize)
			continue
		}
		reader, err := h.fileService.GetFile(ctx, a.URL)
		if err != nil {
			logger.Warnf(ctx, "[ai-chat] skill preview read failed file=%s: %v", a.FileName, err)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(reader, skillPreviewMaxBytes+1))
		_ = reader.Close()
		if err != nil {
			logger.Warnf(ctx, "[ai-chat] skill preview read failed file=%s: %v", a.FileName, err)
			continue
		}
		if int64(len(data)) > skillPreviewMaxBytes {
			logger.Warnf(ctx, "[ai-chat] skill preview skipped (oversize) file=%s size=%d", a.FileName, len(data))
			continue
		}
		previews = append(previews, AIPreview{
			ID:   fmt.Sprintf("file-%d", len(previews)+1),
			Type: "html",
			Data: string(data),
		})
	}
	return previews
}

// skillPreviewLink 生成正文末尾的预览链接。单个产物用固定文案「查看解析结果」；
// 多个产物时后续项加序号区分。链接锚点为 #ai-preview-<id>，与 previews 的 id 对应。
func skillPreviewLink(previews []AIPreview) string {
	if len(previews) == 0 {
		return ""
	}
	if len(previews) == 1 {
		return "[查看解析结果](#ai-preview-" + previews[0].ID + ")"
	}
	parts := make([]string, 0, len(previews))
	for i, p := range previews {
		label := "查看解析结果"
		if i > 0 {
			label = fmt.Sprintf("查看解析结果 %d", i+1)
		}
		parts = append(parts, "["+label+"](#ai-preview-"+p.ID+")")
	}
	return strings.Join(parts, "\n")
}

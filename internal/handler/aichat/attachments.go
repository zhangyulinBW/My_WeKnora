package aichat

// 本文件实现 ai_chat 消息的内联附件处理：base64 图片与文件的解码、校验、
// 落存储与内容提取。处理复用官方会话接口的 AttachmentProcessor（保存 + 抽取
// 文本/转写），保证 pdf/docx/音频等类型与 /agent-chat 行为一致；结果写入
// AIChatRequest 的服务端字段，由 agent_turn 注入 QARequest 并随用户消息落库。
//
// 与官方接口的差异：附件在 SSE 建立前同步处理，失败返回 HTTP 400（官方在
// pre-stream parseQARequest 阶段做同样的校验）；图片存储固定使用全局默认
// FileService，不走租户/智能体级 ImageStorageProvider 解析。

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/Tencent/WeKnora/internal/handler/session"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

const (
	// 与官方会话接口一致的附件配额（qa.go / image_upload.go）。
	maxImageUploads        = 5
	maxImageUploadBytes    = 10 << 20 // 10MB per image
	maxAttachmentUploads   = 5
	maxAttachmentTotalByte = int64(100 * 1024 * 1024)
)

// processUploads 处理 message 请求携带的内联图片与文件附件，结果挂到 req 的
// 服务端字段上。任何失败都终止请求（调用方返回 400），部分成功的存储写入
// 成为孤儿文件，与官方接口的语义一致。
func (h *AIChatHandler) processUploads(
	ctx context.Context,
	req *AIChatRequest,
	agent *types.CustomAgent,
	tenantID uint64,
) error {
	if len(req.Images) > 0 {
		if agent == nil || !agent.Config.ImageUploadEnabled {
			return fmt.Errorf("image upload is not enabled for this agent")
		}
		if err := h.processImages(ctx, req, agent, tenantID); err != nil {
			return err
		}
	}
	if len(req.Attachments) > 0 {
		if err := h.processAttachments(ctx, req, agent, tenantID); err != nil {
			return err
		}
	}
	req.hasUploads = len(req.imageURLs) > 0 || len(req.processedAttachments) > 0
	return nil
}

// processImages 解码并保存内联图片：数据 URI 保留用于视觉模型直传
// （QARequest.ImageURLs），存储 URL 持久化到用户消息。当对话模型不支持视觉
// 且配置了 VLM 模型时，同步生成图片描述作为降级输入。
func (h *AIChatHandler) processImages(
	ctx context.Context,
	req *AIChatRequest,
	agent *types.CustomAgent,
	tenantID uint64,
) error {
	if len(req.Images) > maxImageUploads {
		return fmt.Errorf("too many images, max %d", maxImageUploads)
	}

	decoded := make([][]byte, 0, len(req.Images))
	exts := make([]string, 0, len(req.Images))
	dataURIs := make([]string, 0, len(req.Images))
	for i, img := range req.Images {
		if strings.TrimSpace(img.Data) == "" {
			return fmt.Errorf("image %d has no data", i+1)
		}
		imgBytes, ext, err := decodeImageDataURI(img.Data)
		if err != nil {
			return fmt.Errorf("decode image %d: %w", i+1, err)
		}
		if len(imgBytes) > maxImageUploadBytes {
			return fmt.Errorf("image %d too large (%d bytes, max %d)", i+1, len(imgBytes), maxImageUploadBytes)
		}
		decoded = append(decoded, imgBytes)
		exts = append(exts, ext)
		dataURIs = append(dataURIs, img.Data)
	}

	images := make(types.MessageImages, len(decoded))
	for i, imgBytes := range decoded {
		storedName := fmt.Sprintf("chat-images/%s%s", uuid.New().String(), exts[i])
		fileURL, err := h.fileService.SaveBytes(ctx, imgBytes, tenantID, storedName, false)
		if err != nil {
			return fmt.Errorf("save image %d: %w", i+1, err)
		}
		images[i] = types.MessageImage{URL: fileURL}
	}

	// 对话模型不支持视觉时图片无法直传，改用 VLM 生成文字描述
	//（AgentQA 只在 ImageDescription 非空时拼接降级内容）。
	if !h.chatModelSupportsVision(ctx, agent) {
		if captions := h.analyzeImages(ctx, agent, decoded, req.Message); len(captions) > 0 {
			for i, caption := range captions {
				if caption != "" && i < len(images) {
					images[i].Caption = caption
				}
			}
			req.imageDescription = strings.Join(nonEmpty(captions), "\n")
		}
	}

	req.processedImages = images
	req.imageURLs = dataURIs
	return nil
}

// processAttachments 解码并提取文件附件内容：并发调用官方 AttachmentProcessor
// （保存 + 按类型抽取文本/ASR 转写），配额与文件类型限制对齐官方接口。
func (h *AIChatHandler) processAttachments(
	ctx context.Context,
	req *AIChatRequest,
	agent *types.CustomAgent,
	tenantID uint64,
) error {
	if len(req.Attachments) > maxAttachmentUploads {
		return fmt.Errorf("at most %d attachments are allowed per request", maxAttachmentUploads)
	}
	if agent != nil && len(agent.Config.SupportedFileTypes) > 0 {
		for _, att := range req.Attachments {
			ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(att.FileName)), ".")
			if !containsFileType(agent.Config.SupportedFileTypes, ext) {
				return fmt.Errorf("file type %s is not supported by this agent", ext)
			}
		}
	}

	// 先解码再校验实际字节数：客户端申报的 file_size 只是元数据。
	maxSize := secutils.GetMaxFileSize()
	decoded := make([][]byte, len(req.Attachments))
	var total int64
	for i, upload := range req.Attachments {
		data, err := session.DecodeBase64Attachment(upload.Data)
		if err != nil {
			return fmt.Errorf("attachment %d decode failed: %w", i+1, err)
		}
		actual := int64(len(data))
		if actual > maxSize {
			return fmt.Errorf("attachment %d exceeds size limit of %d bytes", i+1, maxSize)
		}
		total += actual
		if total > maxAttachmentTotalByte {
			return fmt.Errorf("attachments exceed total request limit of %d bytes", maxAttachmentTotalByte)
		}
		decoded[i] = data
	}

	// ASR 转写与解析引擎均由智能体配置驱动，与官方 parseQARequest 一致。
	attachmentCtx := ctx
	asrModelID := ""
	if agent != nil {
		if agent.Config.AudioUploadEnabled && agent.Config.ASRModelID != "" {
			asrModelID = agent.Config.ASRModelID
		}
		for _, att := range req.Attachments {
			ext := strings.ToLower(filepath.Ext(att.FileName))
			if engine := agent.Config.ResolveChatParserEngine(ext); engine != "" {
				attachmentCtx = context.WithValue(attachmentCtx, types.ChatParserEngineContextKey, engine)
				break
			}
		}
	}

	attachments := make(types.MessageAttachments, len(req.Attachments))
	var wg sync.WaitGroup
	errChan := make(chan error, len(req.Attachments))
	for i, upload := range req.Attachments {
		wg.Add(1)
		go func(idx int, att AIAttachmentUpload) {
			defer wg.Done()
			processed, err := h.attachmentProcessor.ProcessAttachment(
				attachmentCtx, decoded[idx], att.FileName, int64(len(decoded[idx])), tenantID, asrModelID,
			)
			if err != nil {
				errChan <- fmt.Errorf("attachment %d processing failed: %w", idx+1, err)
				return
			}
			attachments[idx] = *processed
		}(i, upload)
	}
	wg.Wait()
	close(errChan)
	if len(errChan) > 0 {
		return <-errChan
	}

	req.processedAttachments = attachments
	return nil
}

// chatModelSupportsVision 查询智能体对话模型的视觉能力；查询失败按不支持
// 处理，让 VLM 降级路径兜底而不是静默丢图。
func (h *AIChatHandler) chatModelSupportsVision(ctx context.Context, agent *types.CustomAgent) bool {
	if agent == nil || h.modelService == nil {
		return false
	}
	modelID := h.applyModelOverride(agent).Config.ModelID
	if modelID == "" {
		return false
	}
	modelInfo, err := h.modelService.GetModelByID(ctx, modelID)
	if err != nil || modelInfo == nil {
		return false
	}
	return modelInfo.Parameters.SupportsVision
}

// analyzeImages 用智能体配置的 VLM 模型生成图片描述（与用户问题相关的单次
// 分析）。未配置 VLM 或调用失败时返回 nil，图片仅作为元数据落库。
func (h *AIChatHandler) analyzeImages(
	ctx context.Context,
	agent *types.CustomAgent,
	images [][]byte,
	userQuery string,
) []string {
	vlmModelID := ""
	if agent != nil {
		vlmModelID = agent.Config.VLMModelID
	}
	if vlmModelID == "" || h.modelService == nil {
		logger.Warnf(ctx, "no VLM model configured, skipping image analysis for non-vision chat model")
		return nil
	}
	vlmModel, err := h.modelService.GetVLMModel(ctx, vlmModelID)
	if err != nil {
		logger.Warnf(ctx, "failed to get VLM model %s, skipping image analysis: %v", vlmModelID, err)
		return nil
	}

	prompt := buildImageAnalysisPrompt(userQuery)
	captions := make([]string, len(images))
	for i, imgBytes := range images {
		analysis, err := vlmModel.Predict(ctx, [][]byte{imgBytes}, prompt)
		if err != nil {
			logger.Warnf(ctx, "VLM analysis failed for image %d: %v", i+1, err)
			continue
		}
		captions[i] = analysis
	}
	return captions
}

// buildImageAnalysisPrompt 生成结合用户问题的 VLM 分析提示词，
// 与官方会话接口的 buildImageAnalysisPrompt 保持一致。
func buildImageAnalysisPrompt(userQuery string) string {
	if strings.TrimSpace(userQuery) == "" {
		return "请分析这张图片的内容。如果包含文字，请提取关键文字信息；如果是自然图片，请描述其主要内容。用简洁的中文回答。"
	}
	return fmt.Sprintf(
		"用户的问题是：%s\n\n请分析图片中与用户问题相关的内容。"+
			"如果图片包含文字/文档/表格，请提取与问题相关的关键信息。"+
			"如果是自然图片/截图/图表，请描述与问题相关的视觉内容。"+
			"用简洁的中文回答，只输出分析结果。",
		userQuery,
	)
}

// decodeImageDataURI 解码 data URI 形式的图片，返回字节内容与扩展名。
func decodeImageDataURI(dataURI string) ([]byte, string, error) {
	if !strings.HasPrefix(dataURI, "data:") {
		return nil, "", fmt.Errorf("not a data URI")
	}
	idx := strings.Index(dataURI, ";base64,")
	if idx < 0 {
		return nil, "", fmt.Errorf("unsupported data URI encoding (expected base64)")
	}
	mimeType := dataURI[5:idx]
	decoded, err := base64.StdEncoding.DecodeString(dataURI[idx+8:])
	if err != nil {
		return nil, "", fmt.Errorf("base64 decode: %w", err)
	}
	return decoded, mimeToExt(mimeType), nil
}

func mimeToExt(mime string) string {
	switch strings.ToLower(mime) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

func containsFileType(supported []string, ext string) bool {
	for _, t := range supported {
		if strings.EqualFold(strings.TrimPrefix(t, "."), ext) {
			return true
		}
	}
	return false
}

func nonEmpty(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

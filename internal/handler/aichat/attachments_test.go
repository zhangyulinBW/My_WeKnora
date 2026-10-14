package aichat

// 附件处理的单元测试：配额校验、智能体开关门控、文本附件内容提取、
// 图片保存与数据直传载荷。文件存储用 fake，解析走真实的 AttachmentProcessor。

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/handler/session"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// saveBytesFileService 只实现 SaveBytes 的 FileService fake，保存返回
// 可预测的存储路径；其余方法报错防止误用。
type saveBytesFileService struct {
	saved []string
}

func (f *saveBytesFileService) CheckConnectivity(ctx context.Context) error { return nil }
func (f *saveBytesFileService) SaveFile(ctx context.Context, _ *multipart.FileHeader, _ uint64, _ string) (string, error) {
	return "", fmt.Errorf("not implemented in fake")
}
func (f *saveBytesFileService) SaveBytes(ctx context.Context, _ []byte, _ uint64, fileName string, _ bool) (string, error) {
	f.saved = append(f.saved, fileName)
	return "local://attachments/" + fileName, nil
}
func (f *saveBytesFileService) GetFile(ctx context.Context, _ string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("not implemented in fake")
}
func (f *saveBytesFileService) GetFileURL(ctx context.Context, filePath string) (string, error) {
	return filePath, nil
}
func (f *saveBytesFileService) DeleteFile(ctx context.Context, _ string) error { return nil }
func (f *saveBytesFileService) CopyFile(ctx context.Context, _ string, _ uint64, _ string) (string, error) {
	return "", fmt.Errorf("not implemented in fake")
}

// 编译期保证 fake 实现完整接口。
var _ interfaces.FileService = (*saveBytesFileService)(nil)

func newUploadTestHandler() *AIChatHandler {
	fileSvc := &saveBytesFileService{}
	return &AIChatHandler{
		fileService:         fileSvc,
		attachmentProcessor: session.NewAttachmentProcessor(fileSvc, nil, nil, nil),
	}
}

func pngDataURI() string {
	png := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	return "data:image/png;base64," + png
}

func TestHasInlineUploads(t *testing.T) {
	req := &AIChatRequest{}
	if req.hasInlineUploads() {
		t.Fatal("empty request should have no uploads")
	}
	req.Images = []AIImageAttachment{{Data: pngDataURI()}}
	if !req.hasInlineUploads() {
		t.Fatal("request with image should report uploads")
	}
	req.Images = nil
	req.Attachments = []AIAttachmentUpload{{Data: "aGk=", FileName: "a.txt"}}
	if !req.hasInlineUploads() {
		t.Fatal("request with attachment should report uploads")
	}
}

func TestProcessUploadsRejectsImagesWhenAgentDisabled(t *testing.T) {
	h := newUploadTestHandler()
	req := &AIChatRequest{Images: []AIImageAttachment{{Data: pngDataURI()}}}
	agent := &types.CustomAgent{Config: types.CustomAgentConfig{}} // ImageUploadEnabled=false

	err := h.processUploads(context.Background(), req, agent, 1)
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("expected image-upload-disabled error, got %v", err)
	}
}

func TestProcessUploadsImageCountAndSizeLimits(t *testing.T) {
	h := newUploadTestHandler()
	agent := &types.CustomAgent{Config: types.CustomAgentConfig{ImageUploadEnabled: true}}
	ctx := context.Background()

	tooMany := &AIChatRequest{}
	for i := 0; i < maxImageUploads+1; i++ {
		tooMany.Images = append(tooMany.Images, AIImageAttachment{Data: pngDataURI()})
	}
	if err := h.processUploads(ctx, tooMany, agent, 1); err == nil || !strings.Contains(err.Error(), "too many images") {
		t.Fatalf("expected too-many-images error, got %v", err)
	}

	// base64 每 4 字符解码为 3 字节，多放 1 组保证解码后超过 maxImageUploadBytes。
	bigPNG := "data:image/png;base64," + strings.Repeat("A", (maxImageUploadBytes/3+1)*4)
	if err := h.processUploads(ctx, &AIChatRequest{Images: []AIImageAttachment{{Data: bigPNG}}}, agent, 1); err == nil ||
		!strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected image-too-large error, got %v", err)
	}

	notAURI := &AIChatRequest{Images: []AIImageAttachment{{Data: "not-a-data-uri"}}}
	if err := h.processUploads(ctx, notAURI, agent, 1); err == nil || !strings.Contains(err.Error(), "decode image") {
		t.Fatalf("expected decode error, got %v", err)
	}
}

func TestProcessUploadsImageSavesAndKeepsPayload(t *testing.T) {
	h := newUploadTestHandler()
	dataURI := pngDataURI()
	req := &AIChatRequest{Images: []AIImageAttachment{{Data: dataURI}}}
	agent := &types.CustomAgent{Config: types.CustomAgentConfig{ImageUploadEnabled: true}}

	if err := h.processUploads(context.Background(), req, agent, 1); err != nil {
		t.Fatalf("processUploads failed: %v", err)
	}
	if len(req.imageURLs) != 1 || req.imageURLs[0] != dataURI {
		t.Fatalf("image payload data URI should be preserved for vision models, got %v", req.imageURLs)
	}
	if len(req.processedImages) != 1 || !strings.HasPrefix(req.processedImages[0].URL, "local://attachments/chat-images/") {
		t.Fatalf("image should be persisted with storage URL, got %+v", req.processedImages)
	}
	if !req.hasUploads {
		t.Fatal("hasUploads should be set after processing an image")
	}
}

func TestProcessUploadsTextAttachmentExtractsContent(t *testing.T) {
	h := newUploadTestHandler()
	req := &AIChatRequest{Attachments: []AIAttachmentUpload{{
		Data:     base64.StdEncoding.EncodeToString([]byte("第一行\n第二行\n")),
		FileName: "notes.txt",
	}}}
	agent := &types.CustomAgent{Config: types.CustomAgentConfig{}}

	if err := h.processUploads(context.Background(), req, agent, 1); err != nil {
		t.Fatalf("processUploads failed: %v", err)
	}
	if len(req.processedAttachments) != 1 {
		t.Fatalf("expected 1 processed attachment, got %d", len(req.processedAttachments))
	}
	att := req.processedAttachments[0]
	if att.FileName != "notes.txt" || att.FileType != ".txt" {
		t.Fatalf("attachment metadata mismatch: %+v", att)
	}
	if att.Content != "第一行\n第二行" {
		t.Fatalf("text content not extracted: %q", att.Content)
	}
	if !req.hasUploads {
		t.Fatal("hasUploads should be set after processing an attachment")
	}
	if len(req.imageURLs) != 0 {
		t.Fatal("no image payloads expected for file attachments")
	}
}

func TestProcessUploadsAttachmentLimits(t *testing.T) {
	h := newUploadTestHandler()
	agent := &types.CustomAgent{Config: types.CustomAgentConfig{}} // 文件附件不要求 ImageUploadEnabled
	ctx := context.Background()

	tooMany := &AIChatRequest{}
	for i := 0; i < maxAttachmentUploads+1; i++ {
		tooMany.Attachments = append(tooMany.Attachments, AIAttachmentUpload{
			Data: base64.StdEncoding.EncodeToString([]byte("x")), FileName: "a.txt",
		})
	}
	if err := h.processUploads(ctx, tooMany, agent, 1); err == nil ||
		!strings.Contains(err.Error(), "at most") {
		t.Fatalf("expected attachment-count error, got %v", err)
	}

	badBase64 := &AIChatRequest{Attachments: []AIAttachmentUpload{{Data: "!!!not-base64!!!", FileName: "a.txt"}}}
	if err := h.processUploads(ctx, badBase64, agent, 1); err == nil ||
		!strings.Contains(err.Error(), "decode failed") {
		t.Fatalf("expected decode error, got %v", err)
	}

	unsupportedType := &AIChatRequest{Attachments: []AIAttachmentUpload{{
		Data: base64.StdEncoding.EncodeToString([]byte("x")), FileName: "a.exe",
	}}}
	if err := h.processUploads(ctx, unsupportedType, agent, 1); err == nil ||
		!strings.Contains(err.Error(), "unsupported file type") {
		t.Fatalf("expected unsupported-type error, got %v", err)
	}
}

func TestProcessUploadsAgentFileTypeRestriction(t *testing.T) {
	h := newUploadTestHandler()
	req := &AIChatRequest{Attachments: []AIAttachmentUpload{{
		Data: base64.StdEncoding.EncodeToString([]byte("x")), FileName: "notes.txt",
	}}}
	agent := &types.CustomAgent{Config: types.CustomAgentConfig{
		SupportedFileTypes: []string{"csv", "xlsx"},
	}}

	err := h.processUploads(context.Background(), req, agent, 1)
	if err == nil || !strings.Contains(err.Error(), "not supported by this agent") {
		t.Fatalf("expected agent file-type restriction error, got %v", err)
	}
}

func TestDecodeImageDataURI(t *testing.T) {
	if _, _, err := decodeImageDataURI("https://example.com/a.png"); err == nil {
		t.Fatal("non-data URI should be rejected")
	}
	if _, _, err := decodeImageDataURI("data:image/png,raw"); err == nil {
		t.Fatal("non-base64 data URI should be rejected")
	}
	data, ext, err := decodeImageDataURI(pngDataURI())
	if err != nil {
		t.Fatalf("valid data URI decode failed: %v", err)
	}
	if ext != ".png" || len(data) == 0 {
		t.Fatalf("unexpected decode result: ext=%q len=%d", ext, len(data))
	}
}

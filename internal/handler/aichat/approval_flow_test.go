package aichat

// 本文件是签核意图的端到端冒烟测试：用前端真实请求体（testdata）驱动
// handleApprovalMessage 全流程——动态字段同步落库、五数组解析、AgentQA
// 元数据注入、模型输出解析、结构卡控、草案组装与单条 approval_draft SSE 事件。
// AgentQA 引擎以伪造 SessionService 代替（直接在总线上发完成事件）。

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// approvalFlowSessionService 伪造会话服务：返回固定会话，并把 AgentQA 替换为
// 「直接发完成事件」的假引擎，同时捕获注入引擎的 QARequest。
type approvalFlowSessionService struct {
	interfaces.SessionService
	session *types.Session
	qaReq   *types.QARequest
	final   string
}

func (f *approvalFlowSessionService) GetSessionByID(_ context.Context, _ uint64, _ string) (*types.Session, error) {
	return f.session, nil
}

func (f *approvalFlowSessionService) CreateSession(_ context.Context, s *types.Session) (*types.Session, error) {
	return s, nil
}

func (f *approvalFlowSessionService) AgentQA(ctx context.Context, req *types.QARequest, bus *event.EventBus) error {
	f.qaReq = req
	return bus.EmitAndWait(ctx, event.Event{
		Type: event.EventAgentComplete,
		Data: event.AgentCompleteData{FinalAnswer: f.final},
	})
}

// approvalFlowMessageService 记录落库的消息（含 approval_context 同步消息）。
type approvalFlowMessageService struct {
	interfaces.MessageService
	created []*types.Message
}

func (f *approvalFlowMessageService) CreateMessage(_ context.Context, m *types.Message) (*types.Message, error) {
	if m.ID == "" {
		m.ID = "msg-" + string(rune('A'+len(f.created)))
	}
	f.created = append(f.created, m)
	return m, nil
}

func (f *approvalFlowMessageService) UpdateMessage(_ context.Context, _ *types.Message) error {
	return nil
}

func (f *approvalFlowMessageService) GetRecentMessagesBySession(_ context.Context, _ string, _ int) ([]*types.Message, error) {
	return nil, nil
}

// runApprovalFlow 用真实请求体（type 改为 message 并带用户需求）驱动完整流程，
// 返回 SSE 响应体与捕获的 QARequest。
func runApprovalFlow(t *testing.T, mutate func(req *AIChatRequest), finalAnswer string) (string, *types.QARequest, *approvalFlowMessageService) {
	t.Helper()
	raw, err := os.ReadFile("testdata/approval_request.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var req AIChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	req.Type = AIRequestTypeMessage
	req.Message = "表单已填写完成，帮我确认送出这张 ECO。"
	if mutate != nil {
		mutate(&req)
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/ai/chat", nil)

	msgSvc := &approvalFlowMessageService{}
	sessSvc := &approvalFlowSessionService{
		session: &types.Session{ID: req.ConversationID, TenantID: 1},
		final:   finalAnswer,
	}
	h := &AIChatHandler{
		config: &config.Config{AIChat: &config.AIChatConfig{
			ApprovalEnabled: true,
			Approval:        &config.AIChatAgentConfig{Name: "approval-test"},
		}},
		sessionService:     sessSvc,
		messageService:     msgSvc,
		customAgentService: &errCustomAgentService{},
		modelService:       &errModelService{},
	}
	h.handleApprovalMessage(context.Background(), c, &req, nil, 1)
	return recorder.Body.String(), sessSvc.qaReq, msgSvc
}

func TestApprovalFlowEndToEndWithRealPayload(t *testing.T) {
	final := `{"message":"正在处理 ECO-000003 的確認送出节点。按你说明表单已填写完成，拟选择確認送出并勾选清单，请核对下方草案。",` +
		`"drafts":[{"activityId":"90A5FEA6080A43CABADFF7BF76C417EA","assignmentId":"27D0EF7E805A445A9C1581833700ABEB",` +
		`"form":{"pathId":"9402D951D1B64F0EABD0FBA7754E9563","comments":"表单已填写完整，确认送出。","checkedTaskIds":["CF55C5DD589C4C98835733DC23CCD7D9"]}}]}`

	body, qaReq, msgSvc := runApprovalFlow(t, nil, final)

	// 结构化上下文注入引擎：任务索引、itemData 原文、激活任务绑定、输入判定。
	if qaReq == nil {
		t.Fatal("AgentQA was not invoked")
	}
	meta := string(qaReq.Metadata)
	for _, want := range []string{
		"approvalTaskIndex", "itemData", "activeTask",
		"90A5FEA6080A43CABADFF7BF76C417EA", "27D0EF7E805A445A9C1581833700ABEB",
		"inputFormReadiness",
	} {
		if !strings.Contains(meta, want) {
			t.Fatalf("QARequest.Metadata missing %q: %s", want, meta)
		}
	}

	frames := parseSSEFrames(t, body)
	var draftFrames []AIEvent
	for _, f := range frames {
		if f.Type == AIEventApprovalDraft {
			draftFrames = append(draftFrames, f)
		}
	}
	if len(draftFrames) != 1 {
		t.Fatalf("approval_draft frames = %d, want 1 (frames: %+v)", len(draftFrames), frames)
	}
	ev := draftFrames[0]
	if !strings.Contains(ev.Message, "ECO-000003") {
		t.Fatalf("message = %q", ev.Message)
	}
	if len(ev.ApprovalDrafts) != 1 {
		t.Fatalf("drafts = %+v", ev.ApprovalDrafts)
	}
	d := ev.ApprovalDrafts[0]
	// 标识全部来自真实任务数据，draftId/revision/status 由后端组装。
	if d.ActivityID != "90A5FEA6080A43CABADFF7BF76C417EA" ||
		d.AssignmentID != "27D0EF7E805A445A9C1581833700ABEB" ||
		d.Form.PathID == nil || *d.Form.PathID != "9402D951D1B64F0EABD0FBA7754E9563" {
		t.Fatalf("draft identifiers not from real task data: %+v", d)
	}
	if d.DraftID == "" || d.Revision != 1 || d.Status != ApprovalStatusEditing {
		t.Fatalf("assembled identifiers = %+v", d)
	}

	// 落库消息：approval_context 同步、用户消息、助手消息。
	roles := map[string]int{}
	for _, m := range msgSvc.created {
		roles[m.Role]++
	}
	if roles[approvalContextRole] != 1 || roles["user"] != 1 || roles["assistant"] != 1 {
		t.Fatalf("persisted roles = %v", roles)
	}
}

func TestApprovalFlowOmitsCarriedFieldsStillWorks(t *testing.T) {
	// 第二轮消息：省略动态字段（沿用已同步值），并回传 editing 草案要求优化。
	final := `{"message":"已按你的修改简化意见。","drafts":[{"activityId":"90A5FEA6080A43CABADFF7BF76C417EA","assignmentId":"27D0EF7E805A445A9C1581833700ABEB","form":{"pathId":"9402D951D1B64F0EABD0FBA7754E9563","comments":"简化后的意见","checkedTaskIds":["CF55C5DD589C4C98835733DC23CCD7D9"]}}]}`
	body, qaReq, _ := runApprovalFlow(t, func(req *AIChatRequest) {
		req.Message = "把意见写简洁一些"
		req.ApprovalTaskIndex = nil // 省略：元数据仍应含索引（沿用同步值需要存量数据，此处验证请求字段为空时流程不失败）
	}, final)

	if qaReq == nil {
		t.Fatal("AgentQA was not invoked")
	}
	frames := parseSSEFrames(t, body)
	found := false
	for _, f := range frames {
		if f.Type == AIEventApprovalDraft && len(f.ApprovalDrafts) == 1 && f.ApprovalDrafts[0].Revision == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no approval_draft with revision 1 in frames: %+v", frames)
	}
}

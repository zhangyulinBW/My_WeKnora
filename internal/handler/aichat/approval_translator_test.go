package aichat

// 本文件测试签核草案翻译器：模型最终答案解析为「正文 + 草案」后组装
// （draftId/revision/status 由代码决定）、单条 approval_draft 终止事件、
// 契约违约（纠正重试不可用时）以带 code 的 error 事件终止。

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// fakeApprovalMessageService 只实现完成路径触达的 UpdateMessage；其余方法经
// 接口内嵌可用（测试不会调用）。
type fakeApprovalMessageService struct {
	interfaces.MessageService
	updated []*types.Message
}

func (f *fakeApprovalMessageService) UpdateMessage(_ context.Context, m *types.Message) error {
	f.updated = append(f.updated, m)
	return nil
}

// errModelService 让纠正重试必然失败（无可用模型），用于验证违约路径
// 终止为 error 事件而不是发出不可用草案。
type errModelService struct {
	interfaces.ModelService
}

func (e *errModelService) GetChatModel(_ context.Context, _ string) (chat.Chat, error) {
	return nil, context.Canceled
}

// fakeSuggestModelService 返回一个固定输出的对话模型（推荐问题生成）。
type fakeSuggestModelService struct {
	interfaces.ModelService
	model chat.Chat
}

func (f *fakeSuggestModelService) GetChatModel(_ context.Context, _ string) (chat.Chat, error) {
	return f.model, nil
}

// fakeSuggestChatModel 对任意对话返回固定文本（Chat 接口三方法直接实现，
// 避免内嵌接口与其同名方法字段冲突）。
type fakeSuggestChatModel struct {
	resp string
}

func (f *fakeSuggestChatModel) Chat(_ context.Context, _ []chat.Message, _ *chat.ChatOptions) (*types.ChatResponse, error) {
	return &types.ChatResponse{Content: f.resp}, nil
}

func (f *fakeSuggestChatModel) ChatStream(_ context.Context, _ []chat.Message, _ *chat.ChatOptions) (<-chan types.StreamResponse, error) {
	return nil, context.Canceled
}

func (f *fakeSuggestChatModel) GetModelName() string { return "fake-suggest" }

func (f *fakeSuggestChatModel) GetModelID() string { return "fake-suggest" }

// errCustomAgentService 让智能体解析失败（推荐问题生成被跳过）。
type errCustomAgentService struct {
	interfaces.CustomAgentService
}

func (e *errCustomAgentService) GetAgentByID(_ context.Context, _ string) (*types.CustomAgent, error) {
	return nil, context.Canceled
}

// newApprovalTestTranslator 构造挂在测试录制器上的签核翻译器。
// customAgentService/modelService 缺省为「解析失败」伪造，推荐问题与纠正
// 重试均被跳过，聚焦草案主路径。
func newApprovalTestTranslator(t *testing.T, deps *approvalDraftDeps, withErrModel bool) (*event.EventBus, *httptest.ResponseRecorder, *fakeApprovalMessageService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/ai/chat", nil)

	msgSvc := &fakeApprovalMessageService{}
	h := &AIChatHandler{
		messageService:     msgSvc,
		customAgentService: &errCustomAgentService{},
	}
	if withErrModel {
		h.modelService = &errModelService{}
	}
	deps.h = h
	req := &AIChatRequest{Version: AIProtocolVersion, ConversationID: "conv-ap", RequestID: "req-ap"}
	done := make(chan struct{})
	base := newAIStreamTranslator(h, c, req, &types.Message{ID: "m1", SessionID: "s1"}, 1, done)
	translator := newApprovalDraftTranslator(base, deps)
	bus := event.NewEventBus()
	translator.subscribe(bus)
	return bus, recorder, msgSvc
}

func approvalTestDeps() *approvalDraftDeps {
	return &approvalDraftDeps{
		agent:   &types.CustomAgent{Config: types.CustomAgentConfig{ModelID: "model-x"}},
		lang:    "zh",
		binding: approvalTestBinding(),
		cfg:     approvalTestConfig(`"1"`),
	}
}

func emitComplete(t *testing.T, bus *event.EventBus, finalAnswer string) {
	t.Helper()
	if err := bus.EmitAndWait(context.Background(), event.Event{
		Type: event.EventAgentComplete,
		Data: event.AgentCompleteData{FinalAnswer: finalAnswer},
	}); err != nil {
		t.Fatalf("emit complete: %v", err)
	}
}

func TestApprovalTranslatorEmitsSingleDraftEvent(t *testing.T) {
	deps := approvalTestDeps()
	bus, recorder, msgSvc := newApprovalTestTranslator(t, deps, false)

	envelope := `{"message":"正在处理 WF-2026-001 的部门签审。意见已预填，请核对。","drafts":[` +
		`{"activityId":"ACT-1","assignmentId":"ASG-1","form":{"pathId":"P_APPROVE","comments":"已核实","checkedTaskIds":["T_1","T_3","T_1"]}}]}`
	emitComplete(t, bus, envelope)

	frames := parseSSEFrames(t, recorder.Body.String())
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want exactly 1 approval_draft: %+v", len(frames), frames)
	}
	ev := frames[0]
	if ev.Type != AIEventApprovalDraft || ev.Message != "正在处理 WF-2026-001 的部门签审。意见已预填，请核对。" {
		t.Fatalf("event = %+v", ev)
	}
	if len(ev.ApprovalDrafts) != 1 {
		t.Fatalf("approvalDrafts = %+v", ev.ApprovalDrafts)
	}
	d := ev.ApprovalDrafts[0]
	if d.DraftID == "" || d.Revision != 1 || d.Status != ApprovalStatusEditing {
		t.Fatalf("draft identifiers must be assembled by backend: %+v", d)
	}
	if d.ActivityID != "ACT-1" || d.AssignmentID != "ASG-1" {
		t.Fatalf("draft binding = %+v", d)
	}
	if len(d.Form.CheckedTaskIDs) != 2 || d.Form.CheckedTaskIDs[0] != "T_1" {
		t.Fatalf("checkedTaskIds not deduped: %v", d.Form.CheckedTaskIDs)
	}
	if ev.Code != "" || ev.Done {
		t.Fatalf("approval_draft must not carry error code / done: %+v", ev)
	}

	// 助手消息落库为「正文 + 组装后草案」的 envelope JSON，支撑多轮优化。
	if len(msgSvc.updated) != 1 {
		t.Fatalf("assistant message updates = %d", len(msgSvc.updated))
	}
	content := msgSvc.updated[0].Content
	if !strings.Contains(content, `"approvalDrafts"`) || !strings.Contains(content, d.DraftID) {
		t.Fatalf("persisted content = %s", content)
	}
	if !msgSvc.updated[0].IsCompleted {
		t.Fatal("assistant message should be marked completed")
	}
}

func TestApprovalTranslatorContinuesEditingInstance(t *testing.T) {
	deps := approvalTestDeps()
	deps.inputDraft = &ApprovalDraft{
		DraftID: "draft-existing", Revision: 1, Status: ApprovalStatusEditing,
		ActivityID: "ACT-1", AssignmentID: "ASG-1",
		Form: &ApprovalForm{PathID: strPtr("P_APPROVE"), Comments: "旧意见", CheckedTaskIDs: []string{"T_1"}},
	}
	bus, recorder, _ := newApprovalTestTranslator(t, deps, false)

	emitComplete(t, bus, `{"message":"已按你的修改简化意见。","drafts":[{"activityId":"ACT-1","assignmentId":"ASG-1","form":{"pathId":"P_APPROVE","comments":"新意见","checkedTaskIds":["T_1","T_3"]}}]}`)

	frames := parseSSEFrames(t, recorder.Body.String())
	if len(frames) != 1 || frames[0].Type != AIEventApprovalDraft {
		t.Fatalf("frames = %+v", frames)
	}
	d := frames[0].ApprovalDrafts[0]
	if d.DraftID != "draft-existing" || d.Revision != 2 {
		t.Fatalf("same instance must keep draftId and bump revision: %+v", d)
	}
}

func TestApprovalTranslatorPlainReplyWithoutDrafts(t *testing.T) {
	deps := approvalTestDeps()
	bus, recorder, _ := newApprovalTestTranslator(t, deps, false)

	emitComplete(t, bus, `{"message":"该节点需要核实技术资料后签核，请补充说明。","drafts":[]}`)

	frames := parseSSEFrames(t, recorder.Body.String())
	if len(frames) != 1 || frames[0].Type != AIEventApprovalDraft {
		t.Fatalf("frames = %+v", frames)
	}
	if frames[0].Message == "" || len(frames[0].ApprovalDrafts) != 0 {
		t.Fatalf("plain reply must carry message with no drafts: %+v", frames[0])
	}
}

func TestApprovalTranslatorContractViolationEmitsCodedError(t *testing.T) {
	deps := approvalTestDeps()
	// withErrModel=true：纠正重试必然失败，违约输出必须以 error 事件终止，
	// 绝不发出绑错任务或含无效 ID 的草案。
	bus, recorder, _ := newApprovalTestTranslator(t, deps, true)

	emitComplete(t, bus, `{"message":"正文","drafts":[{"activityId":"ACT-1","assignmentId":"ASG-1","form":{"pathId":"P_GHOST","checkedTaskIds":["T_GHOST"]}}]}`)

	frames := parseSSEFrames(t, recorder.Body.String())
	if len(frames) != 1 || frames[0].Type != AIEventError {
		t.Fatalf("frames = %+v", frames)
	}
	if frames[0].Code != AIErrorCodeApprovalDraft || !frames[0].Done {
		t.Fatalf("error frame must carry code and done: %+v", frames[0])
	}
	if !strings.Contains(frames[0].Message, "P_GHOST") || !strings.Contains(frames[0].Message, "T_GHOST") {
		t.Fatalf("error message should name the violations: %s", frames[0].Message)
	}
}

func TestApprovalTranslatorInvalidJSONEmitsCodedError(t *testing.T) {
	deps := approvalTestDeps()
	bus, recorder, _ := newApprovalTestTranslator(t, deps, true)

	emitComplete(t, bus, "抱歉，我无法生成 JSON。")

	frames := parseSSEFrames(t, recorder.Body.String())
	if len(frames) != 1 || frames[0].Type != AIEventError || frames[0].Code != AIErrorCodeApprovalDraft {
		t.Fatalf("frames = %+v", frames)
	}
}

// TestApprovalTranslatorAttachesSuggestions 验证推荐问题链路接通：
// 推荐智能体解析成功且模型返回问题时，approval_draft 事件携带 suggestions。
func TestApprovalTranslatorAttachesSuggestions(t *testing.T) {
	deps := approvalTestDeps()
	deps.page = &AIPageContext{PageType: PageTypeProcessApproval}
	deps.itemData = json.RawMessage(`{"masterItem":[]}`)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/ai/chat", nil)

	msgSvc := &fakeApprovalMessageService{}
	h := &AIChatHandler{
		messageService: msgSvc,
		customAgentService: &fakeRecommendAgentService{agent: &types.CustomAgent{
			Config: types.CustomAgentConfig{ModelID: "model-rc"},
		}},
		modelService: &fakeSuggestModelService{model: &fakeSuggestChatModel{
			resp: `{"questions":[{"text":"确认草案后如何完成签核？"},{"text":"清单核验还有哪些未完成？"}]}`,
		}},
	}
	deps.h = h
	req := &AIChatRequest{Version: AIProtocolVersion, ConversationID: "conv-ap", RequestID: "req-ap"}
	done := make(chan struct{})
	base := newAIStreamTranslator(h, c, req, &types.Message{ID: "m1", SessionID: "s1"}, 1, done)
	translator := newApprovalDraftTranslator(base, deps)
	bus := event.NewEventBus()
	translator.subscribe(bus)

	emitComplete(t, bus, `{"message":"正在处理 ECO-000003。","drafts":[{"activityId":"ACT-1","assignmentId":"ASG-1","form":{"pathId":"P_APPROVE","comments":"已核实","checkedTaskIds":["T_1"]}}]}`)

	frames := parseSSEFrames(t, recorder.Body.String())
	if len(frames) != 1 || frames[0].Type != AIEventApprovalDraft {
		t.Fatalf("frames = %+v", frames)
	}
	if len(frames[0].Suggestions) != 2 || !strings.Contains(frames[0].Suggestions[0].Text, "完成签核") {
		t.Fatalf("suggestions = %+v", frames[0].Suggestions)
	}
}

// fakeRecommendAgentService 返回固定推荐智能体。
type fakeRecommendAgentService struct {
	interfaces.CustomAgentService
	agent *types.CustomAgent
}

func (f *fakeRecommendAgentService) GetAgentByID(_ context.Context, _ string) (*types.CustomAgent, error) {
	return f.agent, nil
}

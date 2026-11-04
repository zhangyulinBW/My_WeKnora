package aichat

// 本文件测试 approval_search 意图：页面门控、意图注册与分类映射、任务索引
// 解析/绑定校验，以及用真实请求体驱动端到端的任务筛选流程（模型输出解析、
// 纠正重试、单条 approval_task_selection 事件、缺索引降级）。

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// approvalSearchSessionService 伪造会话服务：返回固定会话。
type approvalSearchSessionService struct {
	interfaces.SessionService
	session *types.Session
}

func (f *approvalSearchSessionService) GetSessionByID(_ context.Context, _ uint64, _ string) (*types.Session, error) {
	return f.session, nil
}

func (f *approvalSearchSessionService) CreateSession(_ context.Context, s *types.Session) (*types.Session, error) {
	return s, nil
}

// approvalSearchMessageService 记录落库消息；最近消息返回空（无存量同步值）。
type approvalSearchMessageService struct {
	interfaces.MessageService
	created []*types.Message
}

func (f *approvalSearchMessageService) CreateMessage(_ context.Context, m *types.Message) (*types.Message, error) {
	if m.ID == "" {
		m.ID = "msg-" + string(rune('A'+len(f.created)))
	}
	f.created = append(f.created, m)
	return m, nil
}

func (f *approvalSearchMessageService) GetRecentMessagesBySession(_ context.Context, _ string, _ int) ([]*types.Message, error) {
	return nil, nil
}

// approvalSearchAgentService 按 ID 返回签核任务筛选智能体；其他 ID 失败，
// 从而让推荐问题生成走降级路径。
type approvalSearchAgentService struct {
	interfaces.CustomAgentService
	id    string
	agent *types.CustomAgent
}

func (f *approvalSearchAgentService) GetAgentByID(_ context.Context, id string) (*types.CustomAgent, error) {
	if id == f.id {
		return f.agent, nil
	}
	return nil, context.Canceled
}

// approvalSearchModelService 返回固定的对话模型，并记录实际使用的模型 ID；
// models 用于校验请求体 model_id 是否为可用 KnowledgeQA 模型。
type approvalSearchModelService struct {
	interfaces.ModelService
	model      chat.Chat
	models     map[string]*types.Model
	gotModelID string
}

func (f *approvalSearchModelService) GetChatModel(_ context.Context, modelID string) (chat.Chat, error) {
	f.gotModelID = modelID
	return f.model, nil
}

func (f *approvalSearchModelService) GetModelByID(_ context.Context, id string) (*types.Model, error) {
	if m, ok := f.models[id]; ok {
		return m, nil
	}
	return nil, context.Canceled
}

// sequenceChatModel 按调用顺序返回预设输出，并记录每次调用的消息（用于断言
// 主路径与纠正重试的上下文一致）。
type sequenceChatModel struct {
	responses []string
	calls     [][]chat.Message
}

func (m *sequenceChatModel) Chat(_ context.Context, messages []chat.Message, _ *chat.ChatOptions) (*types.ChatResponse, error) {
	m.calls = append(m.calls, messages)
	idx := len(m.calls) - 1
	if idx >= len(m.responses) {
		idx = len(m.responses) - 1
	}
	return &types.ChatResponse{Content: m.responses[idx]}, nil
}

func (m *sequenceChatModel) ChatStream(
	_ context.Context,
	_ []chat.Message,
	_ *chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	return nil, context.Canceled
}

func (m *sequenceChatModel) GetModelName() string { return "sequence" }
func (m *sequenceChatModel) GetModelID() string   { return "sequence" }

// runApprovalSearchFlow 用前端的真实请求体（type 改为 message）驱动任务筛选
// 全流程，返回 SSE 响应体、模型调用记录、落库消息与模型服务（用于断言
// 请求体 model_id 是否生效）。
func runApprovalSearchFlow(
	t *testing.T,
	mutate func(req *AIChatRequest),
	responses ...string,
) (string, *sequenceChatModel, *approvalSearchMessageService, *approvalSearchModelService) {
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
	req.Message = "帮我找出这个月未签审的 ECO 任务"
	if mutate != nil {
		mutate(&req)
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/ai/chat", nil)

	agent := &types.CustomAgent{
		ID: "approval-search-agent",
		Config: types.CustomAgentConfig{
			ModelID:      "model-approval-search",
			SystemPrompt: "你是签核任务筛选助手。",
		},
	}
	model := &sequenceChatModel{responses: responses}
	msgSvc := &approvalSearchMessageService{}
	modelSvc := &approvalSearchModelService{
		model: model,
		models: map[string]*types.Model{
			"request-model": {ID: "request-model", Type: types.ModelTypeKnowledgeQA},
		},
	}
	h := &AIChatHandler{
		config: &config.Config{AIChat: &config.AIChatConfig{
			ApprovalSearchAgentID: "approval-search-agent-id",
			ApprovalSearchEnabled: true,
			ModelID:               "config-fallback-model",
		}},
		sessionService: &approvalSearchSessionService{
			session: &types.Session{ID: req.ConversationID, TenantID: 1},
		},
		messageService:     msgSvc,
		customAgentService: &approvalSearchAgentService{id: "approval-search-agent-id", agent: agent},
		modelService:       modelSvc,
	}
	h.handleApprovalSearchMessage(context.Background(), c, &req, nil, 1)
	return recorder.Body.String(), model, msgSvc, modelSvc
}

func TestShouldApprovalSearch(t *testing.T) {
	processApprovalPage := &AIPageContext{PageType: PageTypeProcessApproval, TabID: "gotoProcessApproval"}

	cases := []struct {
		name   string
		config *config.AIChatConfig
		page   *AIPageContext
		want   bool
	}{
		{"enabled on processApproval", &config.AIChatConfig{ApprovalSearchEnabled: true}, processApprovalPage, true},
		{"disabled by switch", &config.AIChatConfig{ApprovalSearchEnabled: false}, processApprovalPage, false},
		{"nil config", nil, processApprovalPage, false},
		{"wrong page", &config.AIChatConfig{ApprovalSearchEnabled: true}, &AIPageContext{PageType: "largeTable"}, false},
		{"no page in request", &config.AIChatConfig{ApprovalSearchEnabled: true}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &AIChatHandler{
				config:         &config.Config{AIChat: c.config},
				sessionService: &errSessionService{},
			}
			req := &AIChatRequest{Page: c.page, ConversationID: "conv-should-approval-search"}
			if got := h.shouldApprovalSearch(context.Background(), req, 1); got != c.want {
				t.Fatalf("shouldApprovalSearch = %v, want %v", got, c.want)
			}
		})
	}
}

func TestApprovalSearchIntentRegistered(t *testing.T) {
	h := &AIChatHandler{intents: newIntentRegistry()}
	h.intents.register(&approvalSearchIntentHandler{h: h})
	if handler := h.intents.resolve(IntentApprovalSearch); handler == nil {
		t.Fatal("approval_search intent not resolvable from registry")
	}
}

func TestClassifyIntentMapsApprovalSearch(t *testing.T) {
	agent := &types.CustomAgent{
		ID:     "intent-agent",
		Config: types.CustomAgentConfig{ModelID: "model-intent", SystemPrompt: "classify"},
	}
	h := &AIChatHandler{
		config:             &config.Config{AIChat: &config.AIChatConfig{IntentAgentID: "intent-agent"}},
		customAgentService: &approvalSearchAgentService{id: "intent-agent", agent: agent},
		modelService: &approvalSearchModelService{
			model: &sequenceChatModel{responses: []string{"approval_search\n"}},
		},
	}
	got, err := h.classifyIntent(
		context.Background(),
		"帮我找出所有ECO任务",
		&AIPageContext{PageType: PageTypeProcessApproval},
		nil,
	)
	if err != nil {
		t.Fatalf("classifyIntent: %v", err)
	}
	if got != IntentApprovalSearch {
		t.Fatalf("intent = %q, want %q", got, IntentApprovalSearch)
	}
}

func TestParseApprovalTaskIndex(t *testing.T) {
	entries, err := parseApprovalTaskIndex(json.RawMessage(`[
		{"activityId":"A","assignmentId":"B","itemKeyedName":"ECO-1"},
		{"activityId":"","assignmentId":"B"}
	]`))
	if err != nil {
		t.Fatalf("parseApprovalTaskIndex: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (incomplete binding skipped)", len(entries))
	}
	if entries[0].activityID != "A" || entries[0].assignmentID != "B" {
		t.Fatalf("entry binding = %+v", entries[0])
	}

	if _, err := parseApprovalTaskIndex(json.RawMessage(`{}`)); err == nil {
		t.Fatal("non-array approvalTaskIndex should fail")
	}
}

func TestSelectApprovalTasks(t *testing.T) {
	entries := []approvalTaskIndexEntry{
		{activityID: "A", assignmentID: "B"},
	}
	selected, problems := selectApprovalTasks([]ApprovalTaskRef{
		{ActivityID: "A", AssignmentID: "B"},
		{ActivityID: " A ", AssignmentID: " B "}, // 去空白后重复，只保留首次
		{ActivityID: "X", AssignmentID: "Y"},     // 不在索引中
		{ActivityID: "A"},                        // 缺少绑定
	}, entries)
	if len(selected) != 1 {
		t.Fatalf("selected = %+v, want 1", selected)
	}
	if selected[0].ActivityID != "A" || selected[0].AssignmentID != "B" {
		t.Fatalf("selected binding = %+v", selected[0])
	}
	if len(problems) != 2 {
		t.Fatalf("problems = %v, want 2", problems)
	}
}

func TestApprovalSearchFlowEndToEnd(t *testing.T) {
	final := `{"message":"找到两笔符合条件的待签核任务。","tasks":[` +
		`{"activityId":"90A5FEA6080A43CABADFF7BF76C417EA","assignmentId":"27D0EF7E805A445A9C1581833700ABEB"},` +
		`{"activityId":"121B8513739B490083A13CB31C9032DB","assignmentId":"C13557AC5B89402C87C3E82D793ACE0F"}]}`

	// 复现前端的「提交 ECO 所有任务」请求：只带 approvalTaskIndex，不带 itemData。
	body, model, msgSvc, _ := runApprovalSearchFlow(t, func(req *AIChatRequest) {
		req.Message = "帮我提交ECO的所有任务"
		req.ItemData = nil
	}, final)

	if len(model.calls) != 1 {
		t.Fatalf("model calls = %d, want 1", len(model.calls))
	}
	if len(model.calls[0]) != 2 || model.calls[0][0].Content != "你是签核任务筛选助手。" {
		t.Fatalf("unexpected model messages: %+v", model.calls[0])
	}
	userContent := model.calls[0][1].Content
	for _, want := range []string{
		"approvalTaskIndex", "90A5FEA6080A43CABADFF7BF76C417EA",
		"dateContext", "defaultTaskStatus",
	} {
		if !strings.Contains(userContent, want) {
			t.Fatalf("user context missing %q: %s", want, userContent)
		}
	}

	frames := parseSSEFrames(t, body)
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want exactly 1 approval_task_selection: %+v", len(frames), frames)
	}
	ev := frames[0]
	if ev.Type != AIEventApprovalTaskSelection || ev.Message != "找到两笔符合条件的待签核任务。" {
		t.Fatalf("event = %+v", ev)
	}
	if len(ev.SelectedTasks) != 2 {
		t.Fatalf("selectedTasks = %d, want 2", len(ev.SelectedTasks))
	}
	if ev.SelectedTasks[0].ActivityID != "90A5FEA6080A43CABADFF7BF76C417EA" ||
		ev.SelectedTasks[0].AssignmentID != "27D0EF7E805A445A9C1581833700ABEB" ||
		ev.SelectedTasks[1].ActivityID != "121B8513739B490083A13CB31C9032DB" ||
		ev.SelectedTasks[1].AssignmentID != "C13557AC5B89402C87C3E82D793ACE0F" {
		t.Fatalf("selectedTasks bindings = %+v", ev.SelectedTasks)
	}

	roles := map[string]int{}
	for _, m := range msgSvc.created {
		roles[m.Role]++
	}
	if roles[approvalContextRole] != 1 || roles["user"] != 1 || roles["assistant"] != 1 {
		t.Fatalf("persisted roles = %v", roles)
	}
	for _, m := range msgSvc.created {
		if m.Role == "assistant" {
			if m.Content != ev.Message {
				t.Fatalf("assistant content = %q, want plain message", m.Content)
			}
			if strings.Contains(m.Content, "selectedTasks") || strings.Contains(m.Content, "activityId") {
				t.Fatalf("assistant message must not persist the envelope: %s", m.Content)
			}
			if !m.IsCompleted {
				t.Fatal("assistant message should be completed")
			}
		}
	}
}

func TestApprovalSearchModelIDSelection(t *testing.T) {
	final := `{"message":"找到两笔符合条件的待签核任务。","tasks":[` +
		`{"activityId":"90A5FEA6080A43CABADFF7BF76C417EA","assignmentId":"27D0EF7E805A445A9C1581833700ABEB"},` +
		`{"activityId":"121B8513739B490083A13CB31C9032DB","assignmentId":"C13557AC5B89402C87C3E82D793ACE0F"}]}`

	t.Run("request model wins", func(t *testing.T) {
		_, _, _, modelSvc := runApprovalSearchFlow(t, func(req *AIChatRequest) {
			req.ModelID = "request-model"
		}, final)
		if modelSvc.gotModelID != "request-model" {
			t.Fatalf("model used = %q, want request-model", modelSvc.gotModelID)
		}
	})

	t.Run("invalid request model falls back to agent model", func(t *testing.T) {
		_, _, _, modelSvc := runApprovalSearchFlow(t, func(req *AIChatRequest) {
			req.ModelID = "unknown-model"
		}, final)
		if modelSvc.gotModelID != "model-approval-search" {
			t.Fatalf("model used = %q, want agent model fallback", modelSvc.gotModelID)
		}
	})
}

func TestApprovalSearchFlowRepairsInvalidTaskRef(t *testing.T) {
	invalid := `{"message":"找到 1 条。","tasks":[{"activityId":"NOT-IN-INDEX","assignmentId":"NOPE"}]}`
	valid := `{"message":"已找到 1 条。","tasks":[` +
		`{"activityId":"90A5FEA6080A43CABADFF7BF76C417EA","assignmentId":"27D0EF7E805A445A9C1581833700ABEB"}]}`

	body, model, _, _ := runApprovalSearchFlow(t, nil, invalid, valid)

	if len(model.calls) != 2 {
		t.Fatalf("model calls = %d, want 2 (initial + repair)", len(model.calls))
	}
	if len(model.calls[1]) != 4 {
		t.Fatalf("repair messages = %d, want 4", len(model.calls[1]))
	}
	if !strings.Contains(model.calls[1][3].Content, "不在 approvalTaskIndex 中") {
		t.Fatalf("repair prompt missing problem: %s", model.calls[1][3].Content)
	}

	frames := parseSSEFrames(t, body)
	if len(frames) != 1 || frames[0].Type != AIEventApprovalTaskSelection {
		t.Fatalf("frames = %+v", frames)
	}
	if len(frames[0].SelectedTasks) != 1 {
		t.Fatalf("selectedTasks = %d, want 1", len(frames[0].SelectedTasks))
	}
}

func TestApprovalSearchFlowRejectsInvalidTaskRefAfterRepair(t *testing.T) {
	invalid := `{"message":"找到 1 条。","tasks":[{"activityId":"NOT-IN-INDEX","assignmentId":"NOPE"}]}`

	body, model, _, _ := runApprovalSearchFlow(t, nil, invalid, invalid)

	if len(model.calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(model.calls))
	}
	frames := parseSSEFrames(t, body)
	if len(frames) != 1 || frames[0].Type != AIEventError {
		t.Fatalf("frames = %+v", frames)
	}
	if frames[0].Code != AIErrorCodeApprovalTask {
		t.Fatalf("error code = %q, want %q", frames[0].Code, AIErrorCodeApprovalTask)
	}
}

func TestApprovalSearchFlowWithoutTaskIndex(t *testing.T) {
	body, model, msgSvc, _ := runApprovalSearchFlow(t, func(req *AIChatRequest) {
		req.ApprovalTaskIndex = nil
	}, `{"message":"should not be called","tasks":[]}`)

	if len(model.calls) != 0 {
		t.Fatalf("model should not be called without a task index, got %d calls", len(model.calls))
	}
	frames := parseSSEFrames(t, body)
	if len(frames) != 1 || frames[0].Type != AIEventApprovalTaskSelection {
		t.Fatalf("frames = %+v", frames)
	}
	if len(frames[0].SelectedTasks) != 0 {
		t.Fatalf("selectedTasks = %d, want 0", len(frames[0].SelectedTasks))
	}
	if !strings.Contains(frames[0].Message, "没有可用的签核任务索引") {
		t.Fatalf("message = %q", frames[0].Message)
	}
	assistantPersisted := false
	for _, m := range msgSvc.created {
		if m.Role == "assistant" && m.Content == frames[0].Message {
			assistantPersisted = true
		}
	}
	if !assistantPersisted {
		t.Fatal("no-index reply should be persisted as assistant message")
	}
}

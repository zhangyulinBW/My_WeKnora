package aichat

// 本文件测试 ai_chat 终止事件结构化载荷（AIChatEventPayload）的序列化与落库：
// 缺失/空事件类型不落库，序列化结果与 SSE 协议字段名一致且可选字段缺省；
// 三个流程的 translator 在成功路径补写载荷、失败路径不写。

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestMarshalAIChatEventPayloadNilSafety(t *testing.T) {
	if got := marshalAIChatEventPayload(nil); got != nil {
		t.Fatalf("nil payload must marshal to nil, got %s", got)
	}
	// 缺少 eventType 视为无载荷：没有终止事件形态就不落库。
	if got := marshalAIChatEventPayload(&AIChatEventPayload{Intent: string(IntentSearch)}); got != nil {
		t.Fatalf("payload without eventType must marshal to nil, got %s", got)
	}
}

func TestMarshalAIChatEventPayloadSearchShape(t *testing.T) {
	b := marshalAIChatEventPayload(&AIChatEventPayload{
		Intent:    string(IntentSearch),
		EventType: AIEventSearchDraft,
		Conditions: []AICondition{
			{Field: "status", Label: "状态", Type: "text", Operator: "eq", Value: "未签审"},
		},
		SearchBody: json.RawMessage(`{"keyword":"eco"}`),
	})
	if b == nil {
		t.Fatal("search payload should marshal")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if _, ok := raw["intent"]; !ok {
		t.Fatalf("payload must always carry intent: %s", b)
	}
	if string(raw["eventType"]) != `"search_draft"` {
		t.Fatalf("eventType = %s", raw["eventType"])
	}
	if _, ok := raw["conditions"]; !ok {
		t.Fatalf("conditions missing: %s", b)
	}
	if string(raw["searchBody"]) != `{"keyword":"eco"}` {
		t.Fatalf("searchBody = %s", raw["searchBody"])
	}
	// 可选载荷缺省：approval/approval_search 字段不应出现。
	for _, key := range []string{"approvalDrafts", "selectedTasks", "suggestions"} {
		if _, ok := raw[key]; ok {
			t.Fatalf("absent field %q must be omitted: %s", key, b)
		}
	}
}

func TestMarshalAIChatEventPayloadRoundTrip(t *testing.T) {
	p := &AIChatEventPayload{
		Intent:        string(IntentApprovalSearch),
		EventType:     AIEventApprovalTaskSelection,
		SelectedTasks: []ApprovalTaskRef{{ActivityID: "ACT-1", AssignmentID: "ASG-1"}},
		Suggestions:   []AISuggestionItem{{ID: "s1", Text: "继续追问"}},
	}
	b := marshalAIChatEventPayload(p)
	if b == nil {
		t.Fatal("payload should marshal")
	}
	var back AIChatEventPayload
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Intent != string(IntentApprovalSearch) || back.EventType != AIEventApprovalTaskSelection {
		t.Fatalf("intent/eventType = %s/%s", back.Intent, back.EventType)
	}
	if len(back.SelectedTasks) != 1 || back.SelectedTasks[0].ActivityID != "ACT-1" {
		t.Fatalf("selectedTasks = %+v", back.SelectedTasks)
	}
	if len(back.Suggestions) != 1 || back.Suggestions[0].Text != "继续追问" {
		t.Fatalf("suggestions = %+v", back.Suggestions)
	}
}

// TestSearchDraftTranslatorPersistsPayload 验证 search 意图成功路径：助手消息
// 先落正文（envelope JSON，现状不变），再补写结构化载荷；载荷与 search_draft
// 事件同构（conditions/searchBody），Content 不被改动。
func TestSearchDraftTranslatorPersistsPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/ai/chat", nil)

	msgSvc := &fakeApprovalMessageService{}
	h := &AIChatHandler{
		messageService:     msgSvc,
		customAgentService: &errCustomAgentService{}, // 推荐问题生成跳过
	}
	deps := &searchDraftDeps{
		h:       h,
		agent:   &types.CustomAgent{Config: types.CustomAgentConfig{ModelID: "model-x"}},
		message: "筛出未签审的 ECO",
		searchFields: []AISearchField{
			{Name: "status", Label: "状态", Type: "text"},
		},
		rules: &AIConditionRules{
			OperatorsByType: map[string][]string{"text": {"eq"}},
		},
		lang: "简体中文",
	}
	req := &AIChatRequest{Version: AIProtocolVersion, ConversationID: "conv-search", RequestID: "req-search"}
	done := make(chan struct{})
	base := newAIStreamTranslator(h, c, req, &types.Message{ID: "m1", SessionID: "s1"}, 1, done)
	translator := newSearchDraftTranslator(base, deps)
	bus := event.NewEventBus()
	translator.subscribe(bus)

	final := `{"message":"已整理出 1 条搜索条件","conditions":[{"field":"status","label":"状态","type":"text","operator":"eq","value":"未签审"}],"searchBody":{"status":"未签审"}}`
	if err := bus.EmitAndWait(context.Background(), event.Event{
		Type: event.EventAgentComplete,
		Data: event.AgentCompleteData{FinalAnswer: final},
	}); err != nil {
		t.Fatalf("emit complete: %v", err)
	}

	frames := parseSSEFrames(t, recorder.Body.String())
	if len(frames) != 1 || frames[0].Type != AIEventSearchDraft {
		t.Fatalf("frames = %+v", frames)
	}

	// 两次落库：第一次正文（envelope JSON，供 refinement 上下文），第二次载荷。
	if len(msgSvc.updated) != 2 {
		t.Fatalf("assistant message updates = %d, want content then payload", len(msgSvc.updated))
	}
	if msgSvc.updated[0].Content != final {
		t.Fatalf("content update = %q, want raw envelope", msgSvc.updated[0].Content)
	}
	var payload AIChatEventPayload
	if err := json.Unmarshal(msgSvc.updated[1].AIStructuredPayload, &payload); err != nil {
		t.Fatalf("decode ai chat payload: %v (raw: %s)", err, msgSvc.updated[1].AIStructuredPayload)
	}
	if payload.Intent != string(IntentSearch) || payload.EventType != AIEventSearchDraft {
		t.Fatalf("payload intent/eventType = %s/%s", payload.Intent, payload.EventType)
	}
	if len(payload.Conditions) != 1 || payload.Conditions[0].Field != "status" {
		t.Fatalf("payload conditions = %+v", payload.Conditions)
	}
	if string(payload.SearchBody) != `{"status":"未签审"}` {
		t.Fatalf("payload searchBody = %s", payload.SearchBody)
	}
	if len(payload.Suggestions) != 0 {
		t.Fatalf("suggestions = %+v, want none", payload.Suggestions)
	}
	// 载荷补写不得改写 Content。
	if msgSvc.updated[1].Content != final {
		t.Fatalf("payload update must keep content, got %q", msgSvc.updated[1].Content)
	}
}

package aichat

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
)

// parseSSEFrames 解析 writeEvent 输出的 SSE 文本，还原事件负载列表。
func parseSSEFrames(t *testing.T, body string) []AIEvent {
	t.Helper()
	var frames []AIEvent
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev AIEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("unmarshal SSE frame %q: %v", line, err)
		}
		frames = append(frames, ev)
	}
	return frames
}

// newTestTranslator 构造一个挂在测试录制器上的翻译器并订阅事件总线。
func newTestTranslator(t *testing.T) (*aiStreamTranslator, *event.EventBus, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/ai/chat", nil)

	req := &AIChatRequest{Version: AIProtocolVersion, ConversationID: "conv-1", RequestID: "req-1"}
	done := make(chan struct{})
	translator := newAIStreamTranslator(
		&AIChatHandler{}, c, req, &types.Message{ID: "m1", SessionID: "s1"}, 1, done)
	bus := event.NewEventBus()
	translator.subscribe(bus)
	return translator, bus, recorder
}

// record_admin_info 的 tool_call 必须翻译成专属 admin_info 事件，
// 且不再为该工具下发通用 tool_call / tool_result 帧；其他工具不受影响。
func TestTranslatorTranslatesRecordAdminInfoIntoAdminInfoEvent(t *testing.T) {
	_, bus, recorder := newTestTranslator(t)
	ctx := context.Background()

	if err := bus.EmitAndWait(ctx, event.Event{
		Type: event.EventAgentToolCall,
		Data: event.AgentToolCallData{
			ToolCallID: "tc-admin",
			ToolName:   agenttools.ToolRecordAdminInfo,
			Arguments:  map[string]interface{}{"category": "sql", "detail": "SELECT 1"},
		},
	}); err != nil {
		t.Fatalf("emit admin tool_call: %v", err)
	}
	if err := bus.EmitAndWait(ctx, event.Event{
		Type: event.EventAgentToolResult,
		Data: event.AgentToolResultData{
			ToolCallID: "tc-admin",
			ToolName:   agenttools.ToolRecordAdminInfo,
			Success:    true,
		},
	}); err != nil {
		t.Fatalf("emit admin tool_result: %v", err)
	}
	if err := bus.EmitAndWait(ctx, event.Event{
		Type: event.EventAgentToolCall,
		Data: event.AgentToolCallData{
			ToolCallID: "tc-other",
			ToolName:   "data_analysis",
			Arguments:  map[string]interface{}{"query": "SELECT 2"},
		},
	}); err != nil {
		t.Fatalf("emit other tool_call: %v", err)
	}

	frames := parseSSEFrames(t, recorder.Body.String())
	var admin AIEvent
	var adminCount int
	for _, f := range frames {
		if f.Type != AIEventAdminInfo {
			continue
		}
		adminCount++
		admin = f
		if f.Data["tool_name"] == agenttools.ToolRecordAdminInfo {
			t.Fatalf("admin_info must not carry a tool_name: %+v", f)
		}
	}
	if adminCount != 1 {
		t.Fatalf("admin_info frames = %d, want 1 (frames: %v)", adminCount, frames)
	}
	if admin.Content != "SELECT 1" || admin.Data["category"] != "sql" || admin.Data["tool_call_id"] != "tc-admin" {
		t.Fatalf("admin_info payload = %+v", admin)
	}

	for _, f := range frames {
		if f.Type != AIEventToolCall && f.Type != AIEventToolResult {
			continue
		}
		if name, _ := f.Data["tool_name"].(string); name == agenttools.ToolRecordAdminInfo {
			t.Fatalf("record_admin_info must not emit a generic %s frame: %+v", f.Type, f)
		}
	}

	foundOther := false
	for _, f := range frames {
		if f.Type == AIEventToolCall && f.Data["tool_name"] == "data_analysis" {
			foundOther = true
		}
	}
	if !foundOther {
		t.Fatal("other tools must keep the generic tool_call frame")
	}
}

// category 缺省回退 notes，空 detail 仍发事件（载荷为空，前端可忽略）。
func TestTranslatorAdminInfoDefaultsCategory(t *testing.T) {
	_, bus, recorder := newTestTranslator(t)

	if err := bus.EmitAndWait(context.Background(), event.Event{
		Type: event.EventAgentToolCall,
		Data: event.AgentToolCallData{
			ToolCallID: "tc-admin",
			ToolName:   agenttools.ToolRecordAdminInfo,
			Arguments:  map[string]interface{}{"detail": "Customer id A714"},
		},
	}); err != nil {
		t.Fatalf("emit admin tool_call: %v", err)
	}

	frames := parseSSEFrames(t, recorder.Body.String())
	if len(frames) != 1 || frames[0].Type != AIEventAdminInfo {
		t.Fatalf("frames = %v, want one admin_info", frames)
	}
	if frames[0].Data["category"] != "notes" || frames[0].Content != "Customer id A714" {
		t.Fatalf("admin_info defaults = %+v", frames[0])
	}
}

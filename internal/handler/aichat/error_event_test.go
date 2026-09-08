package aichat

import (
	"encoding/json"
	"testing"
)

func TestErrorEventUsesStandardStreamErrorEnvelope(t *testing.T) {
	h := &AIChatHandler{}
	req := &AIChatRequest{
		Version:        AIProtocolVersion,
		ConversationID: "conversation-1",
		RequestID:      "request-1",
	}

	ev := h.errorEvent(req, "upstream returned 429")
	if ev.ResponseType != AIEventError || ev.ID != req.RequestID {
		t.Fatalf("standard stream fields = (%q, %q), want (%q, %q)", ev.ResponseType, ev.ID, AIEventError, req.RequestID)
	}
	if ev.Content != "upstream returned 429" || !ev.Done {
		t.Fatalf("error content/done = (%q, %t), want terminal error", ev.Content, ev.Done)
	}
	if got, _ := ev.Data["error"].(string); got != ev.Content {
		t.Fatalf("data.error = %q, want %q", got, ev.Content)
	}

	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal error event: %v", err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal error event: %v", err)
	}
	if payload["response_type"] != "error" {
		t.Fatalf("response_type = %#v, want error", payload["response_type"])
	}
}

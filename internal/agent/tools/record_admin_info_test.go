package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRecordAdminInfoToolDefaults(t *testing.T) {
	tool := NewRecordAdminInfoTool(RecordAdminInfoOptions{})
	if tool.Name() != ToolRecordAdminInfo {
		t.Fatalf("Name() = %q, want %q", tool.Name(), ToolRecordAdminInfo)
	}
	if tool.Description() != DefaultRecordAdminInfoDescription {
		t.Fatal("empty options must fall back to the default description")
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
		t.Fatalf("parameters are not valid JSON: %v", err)
	}
	props, _ := schema["properties"].(map[string]interface{})
	category, _ := props["category"].(map[string]interface{})
	if category == nil {
		t.Fatal("schema is missing the category property")
	}
	if _, hasEnum := category["enum"]; hasEnum {
		t.Fatal("free-form configuration must not inject a category enum")
	}
}

func TestRecordAdminInfoToolDescriptionOverride(t *testing.T) {
	tool := NewRecordAdminInfoTool(RecordAdminInfoOptions{Description: "  custom instructions  "})
	if tool.Description() != "custom instructions" {
		t.Fatalf("Description() = %q, want the trimmed override", tool.Description())
	}
}

func TestRecordAdminInfoToolExecute(t *testing.T) {
	tool := NewRecordAdminInfoTool(RecordAdminInfoOptions{})
	res, err := tool.Execute(context.Background(), json.RawMessage(
		`{"category":"sql","detail":"SELECT 1"}`))
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !res.Success {
		t.Fatalf("Execute failed: %s", res.Error)
	}
	if got := res.Data["display_type"]; got != "admin_info" {
		t.Fatalf("display_type = %v, want admin_info", got)
	}
	if got := res.Data["category"]; got != "sql" {
		t.Fatalf("category = %v, want sql", got)
	}
	if got := res.Data["detail"]; got != "SELECT 1" {
		t.Fatalf("detail = %v, want echo of the input", got)
	}

	// Empty category defaults to notes.
	res, err = tool.Execute(context.Background(), json.RawMessage(`{"detail":"x"}`))
	if err != nil || !res.Success {
		t.Fatalf("empty category must default to notes: err=%v res=%+v", err, res)
	}
	if got := res.Data["category"]; got != "notes" {
		t.Fatalf("default category = %v, want notes", got)
	}

	// Empty detail is rejected so the model retries with actual content.
	res, err = tool.Execute(context.Background(), json.RawMessage(`{"category":"sql"}`))
	if err == nil || res.Success {
		t.Fatal("empty detail must fail the call")
	}
	if !strings.Contains(res.Error, "detail is required") {
		t.Fatalf("error should name the missing field, got %q", res.Error)
	}
}

func TestRecordAdminInfoToolCategoryEnum(t *testing.T) {
	tool := NewRecordAdminInfoTool(RecordAdminInfoOptions{Categories: []string{"sql", "notes"}})

	// The enum reaches the schema offered to the model.
	var schema map[string]interface{}
	if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
		t.Fatalf("parameters are not valid JSON: %v", err)
	}
	props, _ := schema["properties"].(map[string]interface{})
	category, _ := props["category"].(map[string]interface{})
	enum, _ := category["enum"].([]interface{})
	if len(enum) != 2 || enum[0] != "sql" || enum[1] != "notes" {
		t.Fatalf("category enum = %v, want [sql notes]", enum)
	}

	// In-range passes; out-of-range fails with the configured list in the
	// message so the model can retry.
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"category":"notes","detail":"x"}`))
	if err != nil || !res.Success {
		t.Fatalf("valid category must pass: err=%v res=%+v", err, res)
	}
	res, err = tool.Execute(context.Background(), json.RawMessage(`{"category":"misc","detail":"x"}`))
	if err == nil || res.Success {
		t.Fatal("out-of-range category must fail the call")
	}
	if !strings.Contains(res.Error, "sql") || !strings.Contains(res.Error, "notes") {
		t.Fatalf("error should list the configured categories, got %q", res.Error)
	}
}

func TestCompactToolSummaryAdminInfo(t *testing.T) {
	got := compactToolSummary(true, "", map[string]interface{}{
		"display_type": "admin_info",
		"category":     "sql",
	})
	if got != "Recorded admin info (sql)" {
		t.Fatalf("compactToolSummary() = %q, want Recorded admin info (sql)", got)
	}
}

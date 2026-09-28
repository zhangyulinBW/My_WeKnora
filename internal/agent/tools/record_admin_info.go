package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

// DefaultRecordAdminInfoDescription is the model-facing usage instruction.
// config.admin_info_tool.description overrides it without recompiling.
const DefaultRecordAdminInfoDescription = `Use this tool to record internal, administrator-facing details so they can be shown to administrators in a collapsed panel instead of polluting the user-facing answer.

Record with it: executed SQL statements, document/knowledge IDs (dN handles), real table and column (property) names, schema facts, page context metadata, failed queries and how they were fixed.

Call it right after each tool use that produced such a fact, BEFORE writing the final answer. One topic per call: category classifies the record (e.g. sql, fields, ids, schema, notes), detail carries the fact as plain text.

The final answer must never contain these internal details and must never mention this tool. If the user asks for the SQL or internal field names, say they are recorded for administrators.`

// RecordAdminInfoOptions carries the deployment-configurable settings from
// config.admin_info_tool (see internal/config/config.go).
type RecordAdminInfoOptions struct {
	// Description overrides the model-facing tool description. Empty uses
	// DefaultRecordAdminInfoDescription.
	Description string
	// Categories optionally restricts the category parameter to an enum,
	// both in the JSON Schema offered to the model and at execution time.
	// Empty allows any non-empty category string.
	Categories []string
}

// RecordAdminInfoTool records administrator-facing details. It has no side
// effects: the recorded facts live in the tool-call arguments, which is where
// frontends read them to render the collapsed admin panel.
type RecordAdminInfoTool struct {
	BaseTool
	options RecordAdminInfoOptions
}

// RecordAdminInfoInput defines the input parameters. Detail carries one
// internal fact per call; Category is optional and defaults to "notes".
type RecordAdminInfoInput struct {
	Category string `json:"category,omitempty" jsonschema:"Classification of the record, e.g. sql, fields, ids, schema, notes"`
	Detail   string `json:"detail" jsonschema:"One internal fact worth keeping for administrators, as plain text"`
}

// NewRecordAdminInfoTool creates a record_admin_info tool instance.
func NewRecordAdminInfoTool(options RecordAdminInfoOptions) *RecordAdminInfoTool {
	description := strings.TrimSpace(options.Description)
	if description == "" {
		description = DefaultRecordAdminInfoDescription
	}
	return &RecordAdminInfoTool{
		BaseTool: BaseTool{
			name:        ToolRecordAdminInfo,
			description: description,
			schema:      buildRecordAdminInfoSchema(options.Categories),
		},
		options: options,
	}
}

// Execute validates and echoes the record. Success means "recorded"; the
// authoritative payload for consumers is the tool-call arguments plus the
// Data map returned here.
func (t *RecordAdminInfoTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var input RecordAdminInfoInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to parse args: %v", err),
		}, err
	}

	input.Category = strings.TrimSpace(input.Category)
	if input.Category == "" {
		input.Category = "notes"
	}
	detail := strings.TrimSpace(input.Detail)
	if detail == "" {
		return &types.ToolResult{
			Success: false,
			Error:   "detail is required: record one internal fact per call",
		}, fmt.Errorf("record_admin_info: empty detail")
	}
	if categories := t.options.Categories; len(categories) > 0 && !slices.Contains(categories, input.Category) {
		return &types.ToolResult{
			Success: false,
			Error: fmt.Sprintf("category %q is not one of the configured categories %v; retry with a valid category",
				input.Category, categories),
		}, fmt.Errorf("record_admin_info: invalid category %q", input.Category)
	}

	return &types.ToolResult{
		Success: true,
		Output:  fmt.Sprintf("Recorded admin info (%s).", input.Category),
		Data: map[string]interface{}{
			"display_type": "admin_info",
			"category":     input.Category,
			"detail":       detail,
			"recorded":     true,
		},
	}, nil
}

// buildRecordAdminInfoSchema generates the base schema and, when an enum is
// configured, injects it into the category property so the registry's schema
// validation rejects out-of-list categories before execution.
func buildRecordAdminInfoSchema(categories []string) json.RawMessage {
	schema := utils.GenerateSchema[RecordAdminInfoInput]()
	if len(categories) == 0 {
		return schema
	}
	var m map[string]interface{}
	if err := json.Unmarshal(schema, &m); err != nil {
		return schema
	}
	props, _ := m["properties"].(map[string]interface{})
	category, _ := props["category"].(map[string]interface{})
	if category == nil {
		return schema
	}
	enumValues := make([]interface{}, len(categories))
	for i, c := range categories {
		enumValues[i] = c
	}
	category["enum"] = enumValues
	out, err := json.Marshal(m)
	if err != nil {
		return schema
	}
	return out
}

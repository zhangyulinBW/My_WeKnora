package aichat

// 本文件定义 ai_chat 接口的协议类型与常量。
// 这些类型对应 /api/v1/ai/chat 的请求体与 SSE 事件负载（协议版本 1.0），
// 是前后端契约，字段的 json tag 不能随意改动。

import "encoding/json"

// AIChatRequest is the request body for the custom /api/v1/ai/chat endpoint.
// It is a self-describing, versioned protocol (v1.0) where `type` discriminates
// the request kind: "message" (a new user message) or "search_context" (the
// frontend supplying search fields + condition rules after a search task was
// recognised).
type AIChatRequest struct {
	Version        string            `json:"version"`
	Type           string            `json:"type"`                   // "message" | "search_context"
	ConversationID string            `json:"conversationId"`         // opaque frontend-managed session id
	RequestID      string            `json:"requestId"`              // per-send correlation id (echoed back)
	ActionID       string            `json:"actionId,omitempty"`     // search task id, only for search_context
	Message        string            `json:"message,omitempty"`      // user text (message only)
	Page           *AIPageContext    `json:"page,omitempty"`         // page the user is on
	SearchFields   []AISearchField   `json:"searchFields,omitempty"` // searchable table headers (search_context only)
	SearchBody     json.RawMessage   `json:"searchBody,omitempty"`   // current applied search state (for "what is being queried" questions)
	ConditionRules *AIConditionRules `json:"conditionRules,omitempty"`
}

// AIPageContext identifies the page/tab the user is interacting with.
type AIPageContext struct {
	TabID          string `json:"tabId"`
	PageType       string `json:"pageType"` // e.g. "largeTable"
	ItemTypeID     string `json:"itemTypeId"`
	ItemTypeName   string `json:"itemTypeName"`
	ContextVersion string `json:"contextVersion"`
}

// AISearchField is a single searchable header of the current item type.
type AISearchField struct {
	Name    string          `json:"name"`              // field name used by the search API
	Label   string          `json:"label"`             // human-readable display name
	Type    string          `json:"type"`              // text | number | date | select | boolean
	Options []AIFieldOption `json:"options,omitempty"` // legal enum values for select fields
}

// AIFieldOption is one legal value for a select field.
type AIFieldOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// AIConditionRules is the frontend-maintained "natural language -> machine
// rule" spec used both to steer the model and to validate its output.
type AIConditionRules struct {
	Version         string                         `json:"version"`
	Skill           string                         `json:"skill"` // natural-language conversion spec
	OperatorsByType map[string][]string            `json:"operatorsByType"`
	Operators       map[string]AIConditionOperator `json:"operators"`
	DateContext     AIDateContext                  `json:"dateContext"`
	ResultShape     AIResultShape                  `json:"resultShape"`
	NoFieldMatch    AINoFieldMatch                 `json:"noFieldMatch"`
}

// AIConditionOperator describes an operator's semantics and its search-API encoding.
type AIConditionOperator struct {
	APIValue    string `json:"apiValue"`
	Description string `json:"description"`
}

// AIDateContext is the base for computing relative dates.
type AIDateContext struct {
	CurrentDate       string `json:"currentDate"`
	Timezone          string `json:"timezone"`
	RelativeMonthMode string `json:"relativeMonthMode"`
	ValueFormat       string `json:"valueFormat"`
}

// AIResultShape lists the fields every generated condition must carry.
type AIResultShape struct {
	Required []string `json:"required"`
}

// AINoFieldMatch describes what to do when no search field matches.
type AINoFieldMatch struct {
	Action      string `json:"action"`
	Description string `json:"description"`
}

// AICondition is one parsed condition returned by the model.
type AICondition struct {
	Field        string      `json:"field"`
	Label        string      `json:"label"`
	Type         string      `json:"type"`
	Operator     string      `json:"operator"`
	Value        interface{} `json:"value"`
	DisplayValue string      `json:"displayValue,omitempty"`
}

// AISearchResult is the parsed model output for a search task.
type AISearchResult struct {
	Conditions []AICondition   `json:"conditions"`
	SearchBody json.RawMessage `json:"searchBody"`
}

// AIEvent is the SSE event payload emitted by this endpoint. Fields are
// optional so each event kind carries only the fields it needs.
type AIEvent struct {
	Version        string                 `json:"version"`
	Type           string                 `json:"type"`
	ConversationID string                 `json:"conversationId"`
	RequestID      string                 `json:"requestId"`
	ActionID       string                 `json:"actionId,omitempty"`
	Content        string                 `json:"content,omitempty"`
	Message        string                 `json:"message,omitempty"`
	Done           bool                   `json:"done,omitempty"` // stream-end marker for answer/thinking chunks
	Data           map[string]interface{} `json:"data,omitempty"` // tool metadata / references / step info
	Suggestions    []AISuggestionItem     `json:"suggestions,omitempty"`
	Required       []string               `json:"required,omitempty"`
	Conditions     []AICondition          `json:"conditions,omitempty"`
	SearchBody     json.RawMessage        `json:"searchBody,omitempty"`
	NeedConfirm    *bool                  `json:"needConfirm,omitempty"`
	Unmatched      []AIUnmatched          `json:"unmatchedConditions,omitempty"`
}

// AIUnmatched describes a condition fragment that could not be safely mapped.
type AIUnmatched struct {
	SourceText string `json:"sourceText"`
	Reason     string `json:"reason"`
}

// AISuggestionItem is one recommended/candidate question returned by the
// "start" flow so the user can pick a follow-up.
type AISuggestionItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// AI event `type` values.
const (
	AIEventAnswerChunk       = "answer_chunk"
	AIEventAnswerDone        = "answer_done"
	AIEventThinking          = "thinking"    // agent reasoning step
	AIEventToolCall          = "tool_call"   // agent tool invocation
	AIEventToolResult        = "tool_result" // agent tool result
	AIEventReferences        = "references"  // knowledge references
	AIEventReflection        = "reflection"  // agent reflection step
	AIEventNeedSearchContext = "need_search_context"
	AIEventSearchDraft       = "search_draft"
	AIEventNeedClarification = "need_clarification"
	AIEventSuggestions       = "suggestions"
	AIEventError             = "error"
)

// AI request `type` values.
const (
	AIRequestTypeStart         = "start"
	AIRequestTypeMessage       = "message"
	AIRequestTypeSearchContext = "search_context"
)

// Default protocol version.
const AIProtocolVersion = "1.0"

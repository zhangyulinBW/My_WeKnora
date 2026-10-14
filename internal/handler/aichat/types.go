package aichat

// 本文件定义 ai_chat 接口的协议类型与常量。
// 这些类型对应 /api/v1/ai/chat 的请求体与 SSE 事件负载（协议版本 1.0），
// 是前后端契约，字段的 json tag 不能随意改动。

import (
	"encoding/json"

	"github.com/Tencent/WeKnora/internal/types"
)

// AIChatRequest is the request body for the custom /api/v1/ai/chat endpoint.
// It is a self-describing, versioned protocol (v1.0) where `type` discriminates
// the request kind: "message" (a new user message) or "search_context" (the
// frontend supplying search fields + condition rules after a search task was
// recognised).
type AIChatRequest struct {
	Version        string               `json:"version"`
	Type           string               `json:"type"`                   // "message" | "search_context"
	ConversationID string               `json:"conversationId"`         // opaque frontend-managed session id
	RequestID      string               `json:"requestId"`              // per-send correlation id (echoed back)
	ActionID       string               `json:"actionId,omitempty"`     // search task id, only for search_context
	Message        string               `json:"message,omitempty"`      // user text (message only)
	Images         []AIImageAttachment  `json:"images,omitempty"`       // inline base64 images (message only)
	Attachments    []AIAttachmentUpload `json:"attachments,omitempty"`  // inline base64 file attachments (message only)
	Page           *AIPageContext       `json:"page,omitempty"`         // page the user is on
	SearchFields   []AISearchField      `json:"searchFields,omitempty"` // searchable table headers (search_context only)
	SearchBody     json.RawMessage      `json:"searchBody,omitempty"`   // current applied search state (for "what is being queried" questions)
	ItemData       json.RawMessage      `json:"itemdata,omitempty"`     // 页面数据载荷：singleData 为单条记录，largeTable 等亦可携带表格数据
	ConditionRules *AIConditionRules    `json:"conditionRules,omitempty"`

	// 以下字段由服务端在入口处理后携带，不来自请求 JSON，也不会回发。
	// processedAttachments / processedImages 是已落存储的附件与图片（随用户消息持久化），
	// imageURLs 是可直接喂给视觉模型的数据 URI，imageDescription 是非视觉模型下的
	// VLM 降级描述。见 attachments.go。
	processedAttachments types.MessageAttachments
	processedImages      types.MessageImages
	imageURLs            []string
	imageDescription     string
	hasUploads           bool
}

// hasInlineUploads 报告请求是否携带内联图片或文件（message 阶段才有效）。
func (r *AIChatRequest) hasInlineUploads() bool {
	return len(r.Images) > 0 || len(r.Attachments) > 0
}

// AIImageAttachment 是消息携带的内联图片。客户端只应填 Data（data URI）；
// URL/Caption 由服务端生成，前端传入会被忽略（防 SSRF，与官方会话接口一致）。
type AIImageAttachment struct {
	Data string `json:"data,omitempty"` // base64 data URI (data:image/png;base64,...)
}

// AIAttachmentUpload 是消息携带的内联文件附件。
type AIAttachmentUpload struct {
	Data     string `json:"data"`      // base64-encoded file content
	FileName string `json:"file_name"` // original filename
	FileSize int64  `json:"file_size"` // client-declared size, metadata only
}

// AIPageContext identifies the page/tab the user is interacting with.
type AIPageContext struct {
	TabID          string `json:"tabId"`
	PageType       string `json:"pageType"` // e.g. "largeTable", "singleData"
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
	// DataType 是前端实际使用的类型键（如 "boolean"/"date"/"foreign"）。
	// 部分前端只传 data_type 不传 type，入口处由 normalizeSearchFields
	// 归一化进 Type，后续流程只读 Type。
	DataType string `json:"data_type,omitempty"`
}

// normalizeSearchFields 把前端可能单独携带的 data_type 归一化进 Type，
// 保证条件生成与硬校验拿到的字段类型一致。type 优先，缺失时回填 data_type。
func normalizeSearchFields(fields []AISearchField) {
	for i := range fields {
		if fields[i].Type == "" && fields[i].DataType != "" {
			fields[i].Type = fields[i].DataType
		}
	}
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
// Message is the user-facing summary composed by the model (e.g. "已整理出 N 条
// 搜索条件" / "已刷新"); it is a required field of the output contract whose
// wording rules live in the search agent's system prompt — the handler never
// substitutes its own copy, it retries the model when the field is missing.
type AISearchResult struct {
	Message    string          `json:"message,omitempty"`
	Conditions []AICondition   `json:"conditions"`
	SearchBody json.RawMessage `json:"searchBody"`
}

// AIEvent is the SSE event payload emitted by this endpoint. Fields are
// optional so each event kind carries only the fields it needs.
type AIEvent struct {
	// ID and ResponseType mirror the standard session-stream envelope.  They
	// are populated for terminal errors so clients shared with /agent-chat can
	// render a streamed failure instead of treating the 200 SSE response as an
	// empty answer.
	ID             string                 `json:"id,omitempty"`
	ResponseType   string                 `json:"response_type,omitempty"`
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
	AIEventArtifacts         = "artifacts"   // skill-generated files attached after the turn
	AIEventThinking          = "thinking"    // agent reasoning step
	AIEventToolCall          = "tool_call"   // agent tool invocation
	AIEventToolResult        = "tool_result" // agent tool result
	AIEventAdminInfo         = "admin_info"  // record_admin_info record: content=detail, data={category,tool_call_id}; replaces this tool's generic tool_call/tool_result frames
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

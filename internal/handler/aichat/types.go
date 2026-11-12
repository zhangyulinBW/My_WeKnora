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
	ItemData       json.RawMessage      `json:"itemdata,omitempty"`     // 页面数据载荷：singleData 为单条记录，largeTable 等亦可携带表格数据；processApproval 页为五数组结构（json 反序列化大小写不敏感，前端发的 "itemData" 同样落入本字段）
	// AdditionalItems 是与 itemdata 同级的额外资讯数据，不限制数组还是 JSON
	// 对象，内容结构由前端决定、由 AI 自行分析。它是把补充资讯从 itemdata
	// 中拆出来的独立通道：不携带时各流程行为与只发 itemdata 的历史方式完全
	// 一致（json 反序列化大小写不敏感，前端发的 "AdditionalItems" 同样落入
	// 本字段）。注意与 processApproval 页 itemData 五数组结构里的内层
	// additionalItems 子键无关，两者互不影响。
	AdditionalItems json.RawMessage   `json:"additionalItems,omitempty"`
	ConditionRules  *AIConditionRules `json:"conditionRules,omitempty"`
	// ApprovalTaskIndex 是 processApproval 页的扁平任务索引（整体替换语义）。
	// 仅在该页面类型下使用，其他页面不携带。
	ApprovalTaskIndex json.RawMessage `json:"approvalTaskIndex,omitempty"`

	// ModelID 是本次请求指定的对话模型 ID（来自 GET /api/v1/models 中
	// type 为 KnowledgeQA 的模型）。空值时不指定，回退到各路径自己的
	// 智能体/配置模型。作用于普通对话（normal）与签核任务筛选
	// （approval_search）路径。
	ModelID string `json:"model_id,omitempty"`

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
	// Code 是错误类别的机器可读标识（如 APPROVAL_DRAFT_ERROR /
	// APPROVAL_TASK_ERROR），供前端按类别处理错误；非错误事件不携带。
	Code string `json:"code,omitempty"`
	// ApprovalDrafts 是 approval_draft 事件的载荷：可编辑签审草案数组。
	// 与 search_draft 携带 Conditions 的方式一致，请求侧数据不回显。
	ApprovalDrafts []*ApprovalDraft `json:"approvalDrafts,omitempty"`
	// SelectedTasks 是 approval_task_selection 事件的载荷：模型筛选出的
	// 任务绑定数组，只包含 activityId + assignmentId；前端按绑定在本地
	// 任务索引中定位任务，不在事件里回传完整索引条目。
	SelectedTasks []ApprovalTaskRef `json:"selectedTasks,omitempty"`
	// Previews 是 skill 意图 answer_done 事件携带的会话产物预览（内联完整
	// 内容）。每项 id 对应答案正文里的锚点 #ai-preview-<id>，前端点击锚点时
	// 用对应 preview 的 data 直接渲染（如 iframe srcdoc）。无会话产物时该
	// 字段缺省，answer_done 行为与历史一致。
	Previews []AIPreview `json:"previews,omitempty"`
}

// AIPreview 是 answer_done 事件中一个会话产物的内联预览。
type AIPreview struct {
	// ID 是预览标识（如 "file-1"），与答案正文里的锚点 #ai-preview-<ID> 一一对应。
	ID string `json:"id"`
	// Type 是预览类型，本期只有 "html"；字段保留以扩展 markdown/图片等。
	Type string `json:"type"`
	// Data 是产物的完整内容（type=html 时为完整 HTML 文档）。
	Data string `json:"data"`
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

// AIChatEventPayload 是 ai_chat 终止事件的结构化载荷落库形态（messages
// 表 ai_chat_payload 列）。字段名与 AIEvent 的 SSE json tag 保持一致，
// 前端从官方历史接口（GET /messages/{session_id}/load）读回时按 eventType
// 判别，复用与实时流相同的渲染逻辑。
//
// 只在成功发出终止事件时写入；契约违约与错误轮次不写（nil）。该列不进入
// LLM 上下文（历史构建器只读 Content），也不写入 Content，因此不影响
// approval/approval_search「只存正文纯文本防止 normal 意图模仿 JSON」的设计；
// search 意图的 Content 维持存 envelope JSON 不变（refinement 依赖）。
type AIChatEventPayload struct {
	// Intent 是产生该事件的意图标识（search / approval / approval_search）。
	Intent string `json:"intent"`
	// EventType 是对应的 SSE 终止事件类型（如 search_draft）。
	EventType string `json:"eventType"`
	// Conditions / SearchBody 是 search 意图经硬校验后的条件草稿。
	Conditions []AICondition   `json:"conditions,omitempty"`
	SearchBody json.RawMessage `json:"searchBody,omitempty"`
	// ApprovalDrafts 是 approval 意图后端组装后的签审草案（含 draftId/
	// revision/status），自包含完整表单，可独立回放。
	ApprovalDrafts []*ApprovalDraft `json:"approvalDrafts,omitempty"`
	// SelectedTasks 是 approval_search 意图筛选出的任务绑定（activityId +
	// assignmentId）。回放时前端按绑定在本地任务索引中定位，找不到需容错。
	SelectedTasks []ApprovalTaskRef `json:"selectedTasks,omitempty"`
	// Suggestions 是本轮推荐问题，与终止事件携带的一致。
	Suggestions []AISuggestionItem `json:"suggestions,omitempty"`
}

// marshalAIChatEventPayload 把终止事件载荷序列化为可落库的 types.JSON。
// 载荷缺失（nil/无事件类型）或序列化失败都返回 nil——载荷落库是历史回放
// 的增强能力，任何失败都不允许阻断回复流。
func marshalAIChatEventPayload(p *AIChatEventPayload) types.JSON {
	if p == nil || p.EventType == "" {
		return nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	return types.JSON(b)
}

// AI event `type` values.
const (
	AIEventAnswerChunk           = "answer_chunk"
	AIEventAnswerDone            = "answer_done"
	AIEventArtifacts             = "artifacts"   // skill-generated files attached after the turn
	AIEventThinking              = "thinking"    // agent reasoning step
	AIEventToolCall              = "tool_call"   // agent tool invocation
	AIEventToolResult            = "tool_result" // agent tool result
	AIEventAdminInfo             = "admin_info"  // record_admin_info record: content=detail, data={category,tool_call_id}; replaces this tool's generic tool_call/tool_result frames
	AIEventReferences            = "references"  // knowledge references
	AIEventReflection            = "reflection"  // agent reflection step
	AIEventNeedSearchContext     = "need_search_context"
	AIEventSearchDraft           = "search_draft"
	AIEventNeedClarification     = "need_clarification"
	AIEventApprovalDraft         = "approval_draft"          // 签审轮终止事件：message 承载正文说明，approvalDrafts 承载草案（含 AIassessment），suggestions 携带本轮推荐问题；其后不追加 answer_done
	AIEventApprovalTaskSelection = "approval_task_selection" // 签核任务筛选终止事件：message 承载正文说明，selectedTasks 承载任务绑定（activityId + assignmentId），suggestions 携带本轮推荐问题；其后不追加 answer_done
	AIEventSuggestions           = "suggestions"
	AIEventError                 = "error"
)

// AI request `type` values.
const (
	AIRequestTypeStart         = "start"
	AIRequestTypeMessage       = "message"
	AIRequestTypeSearchContext = "search_context"
)

// Default protocol version.
const AIProtocolVersion = "1.0"

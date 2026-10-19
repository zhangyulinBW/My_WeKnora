package aichat

// 本文件定义 processApproval（流程签审）页面的协议类型：itemData 五数组结构、
// 签审配置/清单/草案表单，以及从任务卡片提取绑定、按活动+指派匹配配置与
// 草案的解析助手。
// masterItem/approvalItem 等原始记录保持 RawMessage 原样透传给模型（含
// @aras.* 属性与空值，不做摘要）；仅参与绑定与校验的 approvalConfig /
// approvalDrafts 使用强类型。字段语义见流程签审协议文档（文档 2 §3、文档 3 §3）。

import (
	"encoding/json"
	"strings"
)

// PageTypeProcessApproval 是流程签审页面的 page.pageType 标识。
const PageTypeProcessApproval = "processApproval"

// 草案状态（协议文档 2 §3）：后端新生成只允许 editing；saved/completed/
// cancelled 由前端在业务操作结束后经下一次 message 报告，form 为 null。
const (
	ApprovalStatusEditing   = "editing"
	ApprovalStatusSaved     = "saved"
	ApprovalStatusCompleted = "completed"
	ApprovalStatusCancelled = "cancelled"
)

// AIErrorCodeApprovalDraft 是签审草案生成失败的错误类别标识（error 事件 code）。
const AIErrorCodeApprovalDraft = "APPROVAL_DRAFT_ERROR"

// ApprovalData 是 processApproval 页 itemData 载荷的五数组结构。
// masterItem 是用户明确添加的待签核任务卡片，0、1 或多笔，按活动+指派去重；
// 左侧当前浏览卡片不等于已添加集合。
type ApprovalData struct {
	MasterItem      []json.RawMessage     `json:"masterItem"`
	AdditionalItems []json.RawMessage     `json:"additionalItems"`
	ApprovalConfig  []ApprovalConfigEntry `json:"approvalConfig"`
	ApprovalItem    []json.RawMessage     `json:"approvalItem"`
	ApprovalDrafts  []ApprovalDraft       `json:"approvalDrafts"`
}

// parseApprovalData 把 itemData 原文解析为五数组结构；空载荷返回空结构
// 而非 nil，调用方统一按「未提供该数组」处理。
func parseApprovalData(raw json.RawMessage) (*ApprovalData, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return &ApprovalData{}, nil
	}
	var data ApprovalData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

// ApprovalConfigEntry 是与激活任务匹配的实际签审上下文，以 activityId +
// assignmentId 与卡片绑定。bcs_comment_required / is_required 按值等于 1
// 判定启用（兼容字符串与数字编码，见 approvalFlagEnabled）。
type ApprovalConfigEntry struct {
	ActivityID         string              `json:"activityId"`
	AssignmentID       string              `json:"assignmentId"`
	BcsCommentRequired json.RawMessage     `json:"bcs_comment_required"`
	Paths              []ApprovalPath      `json:"paths"`
	Tasks              []ApprovalTaskEntry `json:"tasks"`
	CurrentForm        *ApprovalForm       `json:"currentForm"`
}

// ApprovalPath 是一个真实可用的普通签审动作。
type ApprovalPath struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Label string `json:"label"`
}

// ApprovalTaskEntry 是签核清单的一条条目。条目可能随附多语系描述等属性
// （description@aras.lang.*），模型侧读取 itemData 原文，此结构只取校验
// 所需字段。
type ApprovalTaskEntry struct {
	ID          string          `json:"id"`
	Description string          `json:"description"`
	IsRequired  json.RawMessage `json:"is_required"`
}

// ApprovalForm 是草案的完整编辑表单：动作、意见、勾选集合三个字段总是
// 完整出现，不使用局部补丁。pathId 未选为 null，comments 未填为 ""，
// checkedTaskIds 未勾为 []。
type ApprovalForm struct {
	PathID         *string  `json:"pathId"`
	Comments       string   `json:"comments"`
	CheckedTaskIDs []string `json:"checkedTaskIds"`
}

// normalize 保证勾选数组为非 nil 空切片（序列化为 [] 而非 null），
// 并去重保持首次出现顺序。返回自身便于链式调用。
func (f *ApprovalForm) normalize() *ApprovalForm {
	if f == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(f.CheckedTaskIDs))
	ids := make([]string, 0, len(f.CheckedTaskIDs))
	for _, id := range f.CheckedTaskIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	f.CheckedTaskIDs = ids
	return f
}

// ApprovalDraft 是一份草案实例。draftId/revision/status 由后端组装，
// 模型不产出；编辑态携带完整 form，结束态 form 为 null。
type ApprovalDraft struct {
	DraftID      string        `json:"draftId"`
	Revision     int           `json:"revision"`
	ActivityID   string        `json:"activityId"`
	AssignmentID string        `json:"assignmentId"`
	Status       string        `json:"status"`
	Form         *ApprovalForm `json:"form"`
}

// approvalFlagEnabled 按值等于 1 判定任务自带标识是否启用。请求中可能是
// 字符串 "1"、数字 1、"0"、0 或 null；除等于 1 外的任何值（含空值、缺失）
// 都表示未启用，不从卡片标题或节点名称推测。
func approvalFlagEnabled(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case `"1"`, `1`:
		return true
	default:
		return false
	}
}

// ApprovalTaskBinding 是从任务卡片提取的任务绑定标识。任务以
// activityId + assignmentId 唯一绑定（数组下标不参与绑定）；itemId 只用于
// 配对业务物件，不能单独定位某次签审。JSON 标签用于注入提示词元数据。
type ApprovalTaskBinding struct {
	ActivityID    string `json:"activityId"`
	AssignmentID  string `json:"assignmentId"`
	ItemID        string `json:"itemId"`
	ItemKeyedName string `json:"itemKeyedName"`
}

// parseMasterItemBinding 从原始卡片对象提取绑定标识。卡片为 Aras 风格原始
// 对象：act_id 是活动 ID，id["#text"] 是指派 ID，item["#text"] 是业务单 ID。
// 标识不完整时返回 false，调用方按「缺少任务数据」澄清，不生成可操作草案。
func parseMasterItemBinding(raw json.RawMessage) (ApprovalTaskBinding, bool) {
	var item struct {
		ActID string `json:"act_id"`
		ID    struct {
			Text string `json:"#text"`
		} `json:"id"`
		Item struct {
			Text      string `json:"#text"`
			KeyedName string `json:"@aras.keyed_name"`
		} `json:"item"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return ApprovalTaskBinding{}, false
	}
	if item.ActID == "" || item.ID.Text == "" {
		return ApprovalTaskBinding{}, false
	}
	return ApprovalTaskBinding{
		ActivityID:    item.ActID,
		AssignmentID:  item.ID.Text,
		ItemID:        item.Item.Text,
		ItemKeyedName: item.Item.KeyedName,
	}, true
}

// matchConfig 返回与绑定标识（活动+指派同时匹配）对应的签审上下文。
// 上下文缺失表示签审数据未取得，与 paths/tasks 为空数组不是同一情况。
func (d *ApprovalData) matchConfig(b ApprovalTaskBinding) *ApprovalConfigEntry {
	for i := range d.ApprovalConfig {
		cfg := &d.ApprovalConfig[i]
		if cfg.ActivityID == b.ActivityID && cfg.AssignmentID == b.AssignmentID {
			return cfg
		}
	}
	return nil
}

// editingDraft 返回输入草案中处于 editing 状态、且绑定到指定任务的草案。
// 每个任务最多一份可编辑草案；命中时其 form 是该任务本轮人工编辑的完整事实，
// 各笔独立互不影响。
func (d *ApprovalData) editingDraft(b ApprovalTaskBinding) *ApprovalDraft {
	for i := range d.ApprovalDrafts {
		dr := &d.ApprovalDrafts[i]
		if dr.Status == ApprovalStatusEditing &&
			dr.ActivityID == b.ActivityID && dr.AssignmentID == b.AssignmentID {
			return dr
		}
	}
	return nil
}

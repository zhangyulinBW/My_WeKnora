package aichat

// 本文件实现流程签审的机器规则硬校验（协议文档 3 §6）：
//   - validateApprovalDrafts：结构卡控（§6.1）。违反时触发一次纠正重试，
//     仍违反则终止并报 APPROVAL_DRAFT_ERROR，绝不发出绑错任务或含无效 ID 的草案。
//   - computeApprovalReadiness：保存/完成条件的确定性判定（§6.2/§6.3）。
//     结果作为事实注入提示词元数据，正文措辞仍完全由提示词决定。

import "fmt"

// containsPathID 报告 pathId 是否为本任务的真实动作 ID。
func containsPathID(paths []ApprovalPath, pathID string) bool {
	for i := range paths {
		if paths[i].ID == pathID {
			return true
		}
	}
	return false
}

// containsTaskID 报告清单 ID 是否属于本任务的签核清单。
func containsTaskID(tasks []ApprovalTaskEntry, taskID string) bool {
	for i := range tasks {
		if tasks[i].ID == taskID {
			return true
		}
	}
	return false
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// validateApprovalDrafts 对模型产出的草案做结构卡控（文档 3 §6.1）：
// 每份草案的绑定（activityId + assignmentId）必须与某个已添加任务一致且
// 同一任务最多一份；必须存在该任务匹配的签审上下文；pathId 必须属于该任务
// paths 或为 null；checkedTaskIds 必须是该任务全部清单 ID 的子集（去重由
// normalize 处理）。返回违约说明，空切片表示通过。不能为解决结构错误编造
// 任务 ID 或选项。
func validateApprovalDrafts(drafts []approvalModelDraft, tasks *approvalTaskSet) []string {
	var issues []string
	if len(drafts) == 0 {
		return issues
	}
	if tasks.empty() {
		return []string{"当前没有已添加的待签核任务，不能生成可操作草案，drafts 必须为空数组"}
	}
	seen := make(map[approvalBindingKey]int, len(drafts))
	for i, d := range drafts {
		key := approvalBindingKey{activityID: d.ActivityID, assignmentID: d.AssignmentID}
		if prev, dup := seen[key]; dup {
			issues = append(issues, fmt.Sprintf("drafts[%d]：与 drafts[%d] 绑定同一任务，同一任务最多一份草案", i, prev))
			continue
		}
		seen[key] = i
		task := tasks.lookup(d.ActivityID, d.AssignmentID)
		if task == nil {
			issues = append(issues, fmt.Sprintf("drafts[%d]：activityId/assignmentId 不属于本次已添加的任务", i))
			continue
		}
		if task.cfg == nil {
			issues = append(issues, fmt.Sprintf("drafts[%d]：缺少与该任务匹配的签审上下文（approvalConfig）", i))
			continue
		}
		if d.Form == nil {
			issues = append(issues, fmt.Sprintf("drafts[%d]：缺少完整 form", i))
			continue
		}
		if d.Form.PathID != nil && !containsPathID(task.cfg.Paths, *d.Form.PathID) {
			issues = append(issues, fmt.Sprintf("drafts[%d].form.pathId：%q 不是本任务的真实动作 ID", i, *d.Form.PathID))
		}
		for _, id := range d.Form.CheckedTaskIDs {
			if !containsTaskID(task.cfg.Tasks, id) {
				issues = append(issues, fmt.Sprintf("drafts[%d].form.checkedTaskIds：%q 不是本任务的清单条目 ID", i, id))
			}
		}
	}
	return issues
}

// approvalDefaultPath 是保存时页面采用的默认动作（文档 3 §6.3）：form.pathId
// 未选且 paths 存在第一项时，保存采用该项的真实动作。该规则不能推断用户
// 赞成第一项动作，只描述保存行为。
type approvalDefaultPath struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Label string `json:"label"`
}

// approvalReadiness 是按文档 3 §6.2/§6.3 对某个 form 的确定性判定结果。
// MissingForSave / MissingForComplete 是缺失项的人读描述，供模型撰写正文
// 「需补充」部分时引用；保存与完成分别判定，完成条件包含保存条件。
type approvalReadiness struct {
	SaveReady          bool                 `json:"saveReady"`
	CompleteReady      bool                 `json:"completeReady"`
	MissingForSave     []string             `json:"missingForSave,omitempty"`
	MissingForComplete []string             `json:"missingForComplete,omitempty"`
	DefaultSavePath    *approvalDefaultPath `json:"defaultSavePath,omitempty"`
}

// approvalTaskReadiness 给单个任务的输入表单判定补上任务绑定，多笔时模型
// 能逐笔对号入座；approvalReadiness 的字段经内嵌平铺到同一层。
type approvalTaskReadiness struct {
	ActivityID   string `json:"activityId"`
	AssignmentID string `json:"assignmentId"`
	approvalReadiness
}

// computeApprovalReadiness 分别计算保存与完成条件（文档 3 §6.2）：
//   - 意见必填（bcs_comment_required == 1）时保存与完成都要求意见非空；
//     非空按字符串长度判定，不去除首尾空白。
//   - 动作未选不拦截保存；完成要求 pathId 已选且属于本任务真实选项。
//   - 保存允许必勾清单未完成暂存；完成要求每条 is_required == 1 的清单已勾选。
//   - 非必填意见、非必勾清单均不产生缺失项。
//
// form 为 nil 时按全空表单判定（初次生成前对 currentForm/输入草案的评估）。
func computeApprovalReadiness(form *ApprovalForm, cfg *ApprovalConfigEntry) approvalReadiness {
	r := approvalReadiness{}
	if cfg == nil {
		return r
	}
	var f ApprovalForm
	if form != nil {
		f = *form
	}
	if approvalFlagEnabled(cfg.BcsCommentRequired) && len(f.Comments) == 0 {
		r.MissingForSave = append(r.MissingForSave, "签审意见（本任务意见必填，保存与完成都需要填写）")
	}
	// 完成条件在保存条件之上追加动作与必勾清单要求。
	r.MissingForComplete = append(r.MissingForComplete, r.MissingForSave...)
	if f.PathID == nil {
		r.MissingForComplete = append(r.MissingForComplete, "签审动作未选择")
		// 保存的默认动作规则（§6.3）：paths 存在第一项时保存采用之；
		// paths 为空时不虚构默认动作，也不因此增加保存必填。
		if len(cfg.Paths) > 0 {
			p := cfg.Paths[0]
			r.DefaultSavePath = &approvalDefaultPath{ID: p.ID, Name: p.Name, Label: p.Label}
		}
	} else if !containsPathID(cfg.Paths, *f.PathID) {
		r.MissingForComplete = append(r.MissingForComplete, "签审动作不是本任务的真实选项")
	}
	for i := range cfg.Tasks {
		task := &cfg.Tasks[i]
		if !approvalFlagEnabled(task.IsRequired) {
			continue
		}
		if !containsString(f.CheckedTaskIDs, task.ID) {
			r.MissingForComplete = append(r.MissingForComplete, "必勾清单未完成："+approvalTaskLabel(task))
		}
	}
	r.SaveReady = len(r.MissingForSave) == 0
	r.CompleteReady = len(r.MissingForComplete) == 0
	return r
}

// approvalTaskLabel 返回清单条目的人读标识（描述优先，缺失时退回 ID）。
func approvalTaskLabel(task *ApprovalTaskEntry) string {
	if task.Description != "" {
		return task.Description
	}
	return task.ID
}

package aichat

// 本文件用前端实际发送的请求体样例（testdata/approval_request.json，来自
// 流程签审页面的真实抓包）验证协议类型的解析：itemData 五数组结构、
// 任务自带标识的宽容取值、激活卡片绑定提取。

import (
	"encoding/json"
	"os"
	"testing"
)

// loadApprovalFixture 读取真实请求体样例并解析为协议请求结构。
func loadApprovalFixture(t *testing.T) (AIChatRequest, *ApprovalData) {
	t.Helper()
	raw, err := os.ReadFile("testdata/approval_request.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var req AIChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	// 前端发的键是 "itemData"（camelCase），请求结构 tag 为 "itemdata"，
	// 依赖 json 反序列化的大小写不敏感匹配落入同一字段。
	if len(req.ItemData) == 0 {
		t.Fatal("itemData (camelCase key) did not populate req.ItemData")
	}
	data, err := parseApprovalData(req.ItemData)
	if err != nil {
		t.Fatalf("parseApprovalData: %v", err)
	}
	return req, data
}

func TestParseApprovalFixture(t *testing.T) {
	req, data := loadApprovalFixture(t)

	if req.Page == nil || req.Page.PageType != PageTypeProcessApproval {
		t.Fatalf("pageType = %+v, want processApproval", req.Page)
	}
	if len(req.ApprovalTaskIndex) == 0 {
		t.Fatal("approvalTaskIndex missing")
	}
	var index []map[string]interface{}
	if err := json.Unmarshal(req.ApprovalTaskIndex, &index); err != nil {
		t.Fatalf("approvalTaskIndex not a flat array: %v", err)
	}
	if len(index) != 4 {
		t.Fatalf("approvalTaskIndex entries = %d, want 4", len(index))
	}
	first := index[0]
	for _, key := range []string{"activityId", "assignmentId", "itemId", "itemKeyedName", "menuPaths", "state"} {
		if _, ok := first[key]; !ok {
			t.Errorf("approvalTaskIndex[0] missing key %q", key)
		}
	}

	// 五数组：masterItem 1、approvalConfig 1、approvalItem 1，其余为空。
	if len(data.MasterItem) != 1 || len(data.ApprovalConfig) != 1 || len(data.ApprovalItem) != 1 {
		t.Fatalf("array lengths = master:%d config:%d item:%d, want 1/1/1",
			len(data.MasterItem), len(data.ApprovalConfig), len(data.ApprovalItem))
	}
	if len(data.AdditionalItems) != 0 || len(data.ApprovalDrafts) != 0 {
		t.Fatal("additionalItems/approvalDrafts should be empty")
	}

	// 原始记录保持原文：approvalItem 的嵌套 Relationships 与 @aras.* 属性不丢失。
	var approvalItem map[string]interface{}
	if err := json.Unmarshal(data.ApprovalItem[0], &approvalItem); err != nil {
		t.Fatalf("approvalItem raw: %v", err)
	}
	if approvalItem["@aras.type"] != "Simple ECO" {
		t.Errorf("approvalItem @aras.type = %v, want Simple ECO", approvalItem["@aras.type"])
	}
	rels, ok := approvalItem["Relationships"].(map[string]interface{})
	if !ok {
		t.Fatal("approvalItem Relationships lost")
	}
	if _, ok := rels["Item"]; !ok {
		t.Fatal("approvalItem Relationships.Item lost")
	}

	cfg := data.ApprovalConfig[0]
	if len(cfg.Paths) != 1 || cfg.Paths[0].ID == "" || cfg.Paths[0].Label != "確認送出" {
		t.Fatalf("paths = %+v", cfg.Paths)
	}
	if len(cfg.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(cfg.Tasks))
	}
	if cfg.Tasks[0].ID == "" || cfg.Tasks[0].Description == "" {
		t.Fatalf("task entry incomplete: %+v", cfg.Tasks[0])
	}
	if !approvalFlagEnabled(cfg.Tasks[0].IsRequired) {
		t.Errorf("is_required %q should be enabled", string(cfg.Tasks[0].IsRequired))
	}
	if approvalFlagEnabled(cfg.BcsCommentRequired) {
		t.Errorf("bcs_comment_required %q should be disabled", string(cfg.BcsCommentRequired))
	}
	if cfg.CurrentForm == nil || cfg.CurrentForm.PathID != nil || cfg.CurrentForm.Comments != "" {
		t.Fatalf("currentForm = %+v, want empty initial form", cfg.CurrentForm)
	}

	// 绑定提取：活动/指派/业务单标识与配置条目对得上。
	binding, ok := parseMasterItemBinding(data.MasterItem[0])
	if !ok {
		t.Fatal("parseMasterItemBinding failed on fixture master item")
	}
	if binding.ActivityID != cfg.ActivityID || binding.AssignmentID != cfg.AssignmentID {
		t.Fatalf("binding %+v does not match config %+v", binding, cfg)
	}
	if binding.ItemKeyedName != "ECO-000003" {
		t.Errorf("itemKeyedName = %q, want ECO-000003", binding.ItemKeyedName)
	}
	if data.matchConfig(binding) != &data.ApprovalConfig[0] {
		t.Error("matchConfig did not return the config entry")
	}
	if data.editingDraft(binding) != nil {
		t.Error("editingDraft should be nil when approvalDrafts is empty")
	}
}

func TestApprovalFlagEnabled(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`"1"`, true},
		{`1`, true},
		{`"0"`, false},
		{`0`, false},
		{`null`, false},
		{``, false}, // 字段缺失
		{`"true"`, false},
		{` 1 `, true}, // 容忍空白
	}
	for _, c := range cases {
		if got := approvalFlagEnabled(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("approvalFlagEnabled(%s) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestParseMasterItemBindingIncomplete(t *testing.T) {
	for _, raw := range []string{
		`{}`,
		`{"act_id": "A"}`,                     // 缺指派 ID
		`{"id": {"#text": "S"}}`,              // 缺活动 ID
		`{"act_id": 1, "id": {"#text": "S"}}`, // 类型不符
		`not-json`,
	} {
		if _, ok := parseMasterItemBinding(json.RawMessage(raw)); ok {
			t.Errorf("parseMasterItemBinding(%s) unexpectedly succeeded", raw)
		}
	}
}

func TestApprovalFormNormalize(t *testing.T) {
	f := &ApprovalForm{CheckedTaskIDs: []string{"B", "A", "B", "A"}}
	f.normalize()
	if len(f.CheckedTaskIDs) != 2 || f.CheckedTaskIDs[0] != "B" || f.CheckedTaskIDs[1] != "A" {
		t.Fatalf("normalize kept duplicates or reordered: %v", f.CheckedTaskIDs)
	}
	empty := (&ApprovalForm{}).normalize()
	if empty.CheckedTaskIDs == nil {
		t.Fatal("normalize should produce non-nil empty slice for JSON []")
	}
	b, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"pathId":null,"comments":"","checkedTaskIds":[]}` {
		t.Fatalf("normalized form marshals to %s", b)
	}
}

func TestParseApprovalDataEmpty(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage("null")} {
		data, err := parseApprovalData(raw)
		if err != nil || data == nil {
			t.Fatalf("parseApprovalData(%s) = %v, %v", raw, data, err)
		}
		if len(data.MasterItem) != 0 || len(data.ApprovalConfig) != 0 {
			t.Fatalf("empty payload should yield zero-value data: %+v", data)
		}
	}
	if _, err := parseApprovalData(json.RawMessage(`{"masterItem": "not-array"}`)); err == nil {
		t.Fatal("invalid payload should error")
	}
}

package aichat

// 本文件测试 processApproval 动态字段的合并语义（文档 2 §2.1）：
// 字段出现整体替换、省略沿用、集合 [] 清空、显式 null 视同省略。

import (
	"encoding/json"
	"testing"
)

func TestMergeApprovalContext(t *testing.T) {
	oldIndex := json.RawMessage(`[{"activityId":"A1"}]`)
	oldItem := json.RawMessage(`{"masterItem":[{"act_id":"A1"}]}`)

	t.Run("both replaced", func(t *testing.T) {
		old := &approvalContext{ApprovalTaskIndex: oldIndex, ItemData: oldItem}
		req := &AIChatRequest{
			ApprovalTaskIndex: json.RawMessage(`[{"activityId":"A2"}]`),
			ItemData:          json.RawMessage(`{"masterItem":[]}`),
		}
		merged := mergeApprovalContext(old, req)
		if string(merged.ApprovalTaskIndex) != `[{"activityId":"A2"}]` ||
			string(merged.ItemData) != `{"masterItem":[]}` {
			t.Fatalf("merged = %+v", merged)
		}
	})

	t.Run("omitted fields reuse old values", func(t *testing.T) {
		old := &approvalContext{ApprovalTaskIndex: oldIndex, ItemData: oldItem}
		merged := mergeApprovalContext(old, &AIChatRequest{Message: "只说话不带数据"})
		if string(merged.ApprovalTaskIndex) != string(oldIndex) || string(merged.ItemData) != string(oldItem) {
			t.Fatalf("merged = %+v", merged)
		}
	})

	t.Run("index updated itemData kept", func(t *testing.T) {
		old := &approvalContext{ApprovalTaskIndex: oldIndex, ItemData: oldItem}
		req := &AIChatRequest{ApprovalTaskIndex: json.RawMessage(`[{"activityId":"A2"}]`)}
		merged := mergeApprovalContext(old, req)
		if string(merged.ApprovalTaskIndex) != `[{"activityId":"A2"}]` {
			t.Fatalf("index not replaced: %s", merged.ApprovalTaskIndex)
		}
		if string(merged.ItemData) != string(oldItem) {
			t.Fatalf("itemData must be retained: %s", merged.ItemData)
		}
	})

	t.Run("empty array clears", func(t *testing.T) {
		old := &approvalContext{ApprovalTaskIndex: oldIndex, ItemData: oldItem}
		req := &AIChatRequest{ApprovalTaskIndex: json.RawMessage(`[]`)}
		merged := mergeApprovalContext(old, req)
		if string(merged.ApprovalTaskIndex) != `[]` {
			t.Fatalf("[] should clear the index: %s", merged.ApprovalTaskIndex)
		}
		if string(merged.ItemData) != string(oldItem) {
			t.Fatalf("itemData must be retained: %s", merged.ItemData)
		}
	})

	t.Run("explicit null treated as omitted", func(t *testing.T) {
		old := &approvalContext{ApprovalTaskIndex: oldIndex, ItemData: oldItem}
		req := &AIChatRequest{ItemData: json.RawMessage(`null`)}
		merged := mergeApprovalContext(old, req)
		if string(merged.ItemData) != string(oldItem) {
			t.Fatalf("null itemData should reuse old value: %s", merged.ItemData)
		}
		if string(merged.ApprovalTaskIndex) != string(oldIndex) {
			t.Fatalf("index untouched: %s", merged.ApprovalTaskIndex)
		}
	})

	t.Run("no old state no payload", func(t *testing.T) {
		merged := mergeApprovalContext(nil, &AIChatRequest{})
		if rawPayloadPresent(merged.ApprovalTaskIndex) || rawPayloadPresent(merged.ItemData) {
			t.Fatalf("merged = %+v, want zero value", merged)
		}
	})
}

func TestRawPayloadPresent(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`[{"a":1}]`, true},
		{`[]`, true}, // 空数组是有效载荷（清空语义）
		{`{"a":1}`, true},
		{`null`, false},
		{``, false},
	}
	for _, c := range cases {
		if got := rawPayloadPresent(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("rawPayloadPresent(%s) = %v, want %v", c.raw, got, c.want)
		}
	}
}

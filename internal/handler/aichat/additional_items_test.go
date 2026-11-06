package aichat

// 本文件测试请求顶层 additionalItems（额外资讯，与 itemdata 同级、可为数组
// 或 JSON）的处理：协议绑定（大小写不敏感、数组/对象皆可、不干扰 itemdata）、
// 各分派智能体的元数据注入（normal / search / approval / approval_search）、
// 推荐问题上下文渲染与纠正重试提示词。

import (
	"encoding/json"
	"strings"
	"testing"
)

// 顶层字段大小写不敏感：前端发 "AdditionalItems" 或 "additionalItems" 均落入
// 同一字段；数组与 JSON 对象都允许；不携带时为空且不影响 itemdata。
func TestAIChatRequestAdditionalItemsBinding(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"array form", `"additionalItems":[{"note":"n1"},{"note":"n2"}]`, `[{"note":"n1"},{"note":"n2"}]`},
		{"object form", `"additionalItems":{"source":"erp","rows":3}`, `{"source":"erp","rows":3}`},
		{"capitalized key", `"AdditionalItems":{"k":"v"}`, `{"k":"v"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var req AIChatRequest
			if err := json.Unmarshal([]byte(`{"itemdata":{"id":1},`+c.body+`}`), &req); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if strings.TrimSpace(string(req.AdditionalItems)) != c.want {
				t.Fatalf("AdditionalItems = %s, want %s", req.AdditionalItems, c.want)
			}
			if string(req.ItemData) != `{"id":1}` {
				t.Fatalf("itemdata must be unaffected: %s", req.ItemData)
			}
		})
	}

	t.Run("omitted stays empty", func(t *testing.T) {
		var req AIChatRequest
		if err := json.Unmarshal([]byte(`{"itemdata":{"id":1}}`), &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(req.AdditionalItems) != 0 {
			t.Fatalf("AdditionalItems = %s, want empty", req.AdditionalItems)
		}
	})
}

// normal 流：携带时注入 additionalItems 键，不携带时不注入（与历史行为一致）。
func TestBuildAIMetadataAdditionalItems(t *testing.T) {
	withItems := buildAIMetadata(&AIChatRequest{ItemData: json.RawMessage(`{"id":1}`), AdditionalItems: json.RawMessage(`[{"note":"n"}]`)})
	if !strings.Contains(string(withItems), `"additionalItems":[{"note":"n"}]`) {
		t.Fatalf("metadata missing additionalItems: %s", withItems)
	}
	withoutItems := buildAIMetadata(&AIChatRequest{ItemData: json.RawMessage(`{"id":1}`)})
	if strings.Contains(string(withoutItems), "additionalItems") {
		t.Fatalf("metadata must omit additionalItems when absent: %s", withoutItems)
	}
}

// search 流：主路径元数据与纠正重试提示词均注入额外资讯。
func TestSearchContextAdditionalItems(t *testing.T) {
	items := json.RawMessage(`{"holiday":"2026-01-01"}`)
	meta := buildSearchMetadata(nil, nil, nil, nil, items, nil)
	if !strings.Contains(string(meta), `"additionalItems":{"holiday":"2026-01-01"}`) {
		t.Fatalf("search metadata missing additionalItems: %s", meta)
	}
	if strings.Contains(string(buildSearchMetadata(nil, nil, nil, nil, nil, nil)), "additionalItems") {
		t.Fatal("search metadata must omit additionalItems when absent")
	}

	prompt := buildSearchPrompt("找单", nil, nil, nil, items, nil, "简体中文")
	if !strings.Contains(prompt, `"additionalItems":{"holiday":"2026-01-01"}`) {
		t.Fatalf("search prompt missing additionalItems: %s", prompt)
	}
}

// approval 流：签审草案元数据与任务筛选元数据均注入额外资讯。
func TestApprovalMetadataAdditionalItems(t *testing.T) {
	items := json.RawMessage(`[{"policy":"P-1"}]`)

	draftMeta := buildApprovalMetadata(nil, nil, nil, items, &approvalTaskSet{})
	if !strings.Contains(string(draftMeta), `"additionalItems":[{"policy":"P-1"}]`) {
		t.Fatalf("approval metadata missing additionalItems: %s", draftMeta)
	}
	if strings.Contains(string(buildApprovalMetadata(nil, nil, nil, nil, &approvalTaskSet{})), "additionalItems") {
		t.Fatal("approval metadata must omit additionalItems when absent")
	}

	taskMeta := buildApprovalTaskSearchMetadata(nil, json.RawMessage(`[]`), items)
	if !strings.Contains(string(taskMeta), `"additionalItems":[{"policy":"P-1"}]`) {
		t.Fatalf("approval task metadata missing additionalItems: %s", taskMeta)
	}
}

// 推荐问题上下文：额外资讯以「额外资讯：」段渲染，缺失时不产生空段。
func TestFormatStartDataAdditionalItems(t *testing.T) {
	out := formatStartData(nil, nil, nil, json.RawMessage(`{"id":1}`), json.RawMessage(`[{"note":"n"}]`))
	if !strings.Contains(out, "额外资讯：[{\"note\":\"n\"}]") {
		t.Fatalf("formatStartData missing additionalItems: %s", out)
	}
	without := formatStartData(nil, nil, nil, json.RawMessage(`{"id":1}`), nil)
	if strings.Contains(without, "额外资讯") {
		t.Fatalf("formatStartData must omit 额外资讯 section when absent: %s", without)
	}
}

// processApproval 动态字段同步：additionalItems 同样遵循整体替换/省略沿用语义。
func TestMergeApprovalContextAdditionalItems(t *testing.T) {
	oldItems := json.RawMessage(`[{"note":"old"}]`)
	old := &approvalContext{ItemData: json.RawMessage(`{"masterItem":[]}`), AdditionalItems: oldItems}

	t.Run("replaced", func(t *testing.T) {
		merged := mergeApprovalContext(old, &AIChatRequest{AdditionalItems: json.RawMessage(`[{"note":"new"}]`)})
		if string(merged.AdditionalItems) != `[{"note":"new"}]` {
			t.Fatalf("additionalItems not replaced: %s", merged.AdditionalItems)
		}
	})
	t.Run("omitted reuses old", func(t *testing.T) {
		merged := mergeApprovalContext(old, &AIChatRequest{})
		if string(merged.AdditionalItems) != string(oldItems) {
			t.Fatalf("additionalItems must be retained: %s", merged.AdditionalItems)
		}
	})
	t.Run("explicit null treated as omitted", func(t *testing.T) {
		merged := mergeApprovalContext(old, &AIChatRequest{AdditionalItems: json.RawMessage(`null`)})
		if string(merged.AdditionalItems) != string(oldItems) {
			t.Fatalf("null additionalItems should reuse old value: %s", merged.AdditionalItems)
		}
	})
	t.Run("empty array clears", func(t *testing.T) {
		merged := mergeApprovalContext(old, &AIChatRequest{AdditionalItems: json.RawMessage(`[]`)})
		if string(merged.AdditionalItems) != `[]` {
			t.Fatalf("[] should clear additionalItems: %s", merged.AdditionalItems)
		}
	})
}

package aichat

// 本文件测试流程签审的机器规则：结构卡控（文档 3 §6.1）、保存/完成条件
// 矩阵（§6.2、§6.4 示例）、保存默认动作（§6.3），以及模型输出 envelope 的
// 解析与草案组装。

import (
	"encoding/json"
	"strings"
	"testing"
)

// approvalTestConfig 构造文档 3 §6.4 示例的配置：意见必填、两个动作、
// 三条清单（两条必勾、一条非必勾）。
func approvalTestConfig(commentRequired string) *ApprovalConfigEntry {
	return &ApprovalConfigEntry{
		ActivityID:         "ACT-1",
		AssignmentID:       "ASG-1",
		BcsCommentRequired: json.RawMessage(commentRequired),
		Paths: []ApprovalPath{
			{ID: "P_APPROVE", Name: "Approve", Label: "同意"},
			{ID: "P_REJECT", Name: "Reject", Label: "退回"},
		},
		Tasks: []ApprovalTaskEntry{
			{ID: "T_1", Description: "附件核验", IsRequired: json.RawMessage(`"1"`)},
			{ID: "T_2", Description: "备注核查", IsRequired: json.RawMessage(`"0"`)},
			{ID: "T_3", Description: "价格复核", IsRequired: json.RawMessage(`1`)},
		},
		CurrentForm: &ApprovalForm{},
	}
}

func approvalTestBinding() *ApprovalTaskBinding {
	return &ApprovalTaskBinding{ActivityID: "ACT-1", AssignmentID: "ASG-1", ItemID: "ITEM-1", ItemKeyedName: "WF-2026-001"}
}

func strPtr(s string) *string { return &s }

func TestValidateApprovalDrafts(t *testing.T) {
	binding := approvalTestBinding()
	cfg := approvalTestConfig(`"1"`)
	validForm := &ApprovalForm{PathID: strPtr("P_APPROVE"), Comments: "已核实", CheckedTaskIDs: []string{"T_1", "T_3"}}

	cases := []struct {
		name     string
		drafts   []approvalModelDraft
		binding  *ApprovalTaskBinding
		cfg      *ApprovalConfigEntry
		issues   int    // 期望违约条数
		contains string // 至少包含的违约关键字
	}{
		{"valid", []approvalModelDraft{{ActivityID: "ACT-1", AssignmentID: "ASG-1", Form: validForm}}, binding, cfg, 0, ""},
		{"null pathId allowed", []approvalModelDraft{{ActivityID: "ACT-1", AssignmentID: "ASG-1", Form: &ApprovalForm{}}}, binding, cfg, 0, ""},
		{"unknown pathId", []approvalModelDraft{{ActivityID: "ACT-1", AssignmentID: "ASG-1", Form: &ApprovalForm{PathID: strPtr("P_GHOST")}}}, binding, cfg, 1, "pathId"},
		{"unknown task id", []approvalModelDraft{{ActivityID: "ACT-1", AssignmentID: "ASG-1", Form: &ApprovalForm{CheckedTaskIDs: []string{"T_1", "T_GHOST"}}}}, binding, cfg, 1, "清单条目"},
		{"binding mismatch", []approvalModelDraft{{ActivityID: "ACT-9", AssignmentID: "ASG-9", Form: validForm}}, binding, cfg, 1, "不一致"},
		{"no active task", []approvalModelDraft{{ActivityID: "ACT-1", AssignmentID: "ASG-1", Form: validForm}}, nil, cfg, 1, "没有激活任务"},
		{"no matching config", []approvalModelDraft{{ActivityID: "ACT-1", AssignmentID: "ASG-1", Form: validForm}}, binding, nil, 1, "签审上下文"},
		{"missing form", []approvalModelDraft{{ActivityID: "ACT-1", AssignmentID: "ASG-1"}}, binding, cfg, 1, "form"},
		{"multiple drafts rejected", []approvalModelDraft{
			{ActivityID: "ACT-1", AssignmentID: "ASG-1", Form: validForm},
			{ActivityID: "ACT-1", AssignmentID: "ASG-1", Form: validForm},
		}, binding, cfg, 1, "最多一份"},
		{"empty drafts always ok", nil, nil, nil, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			issues := validateApprovalDrafts(c.drafts, c.binding, c.cfg)
			if len(issues) != c.issues {
				t.Fatalf("issues = %v (%d), want %d", issues, len(issues), c.issues)
			}
			if c.contains != "" && (c.issues == 0 || !strings.Contains(issues[0], c.contains)) {
				t.Fatalf("first issue %q should contain %q", issues[0], c.contains)
			}
		})
	}
}

// TestApprovalReadinessMatrix 覆盖文档 3 §6.4 的示例矩阵（意见必填场景），
// 以及 §6.2 的非必填分支与 §6.3 的保存默认动作。
func TestApprovalReadinessMatrix(t *testing.T) {
	cfg := approvalTestConfig(`"1"`)

	t.Run("comment empty both required checked", func(t *testing.T) {
		r := computeApprovalReadiness(&ApprovalForm{CheckedTaskIDs: []string{"T_1", "T_3"}}, cfg)
		if r.SaveReady || r.CompleteReady {
			t.Fatalf("neither ready: %+v", r)
		}
		if len(r.MissingForSave) != 1 || !strings.Contains(r.MissingForSave[0], "意见") {
			t.Fatalf("missingForSave = %v", r.MissingForSave)
		}
	})

	t.Run("comment filled one required unchecked", func(t *testing.T) {
		r := computeApprovalReadiness(&ApprovalForm{Comments: "已核实附件", CheckedTaskIDs: []string{"T_1"}}, cfg)
		if !r.SaveReady {
			t.Fatalf("save should be ready: %+v", r)
		}
		if r.CompleteReady {
			t.Fatalf("complete should not be ready: %+v", r)
		}
		if !strings.Contains(strings.Join(r.MissingForComplete, ";"), "价格复核") {
			t.Fatalf("missingForComplete = %v", r.MissingForComplete)
		}
	})

	t.Run("all satisfied with path", func(t *testing.T) {
		r := computeApprovalReadiness(&ApprovalForm{PathID: strPtr("P_APPROVE"), Comments: "已核实", CheckedTaskIDs: []string{"T_1", "T_3"}}, cfg)
		if !r.SaveReady || !r.CompleteReady {
			t.Fatalf("both ready: %+v", r)
		}
		if r.DefaultSavePath != nil {
			t.Fatal("default save path must not appear when path selected")
		}
	})

	t.Run("path unselected default rule", func(t *testing.T) {
		r := computeApprovalReadiness(&ApprovalForm{Comments: "已核实", CheckedTaskIDs: []string{"T_1", "T_3"}}, cfg)
		if !r.SaveReady || r.CompleteReady {
			t.Fatalf("save ready, complete not: %+v", r)
		}
		if r.DefaultSavePath == nil || r.DefaultSavePath.ID != "P_APPROVE" || r.DefaultSavePath.Name != "Approve" {
			t.Fatalf("defaultSavePath = %+v, want first real path", r.DefaultSavePath)
		}
	})

	t.Run("comment optional", func(t *testing.T) {
		optional := approvalTestConfig(`"0"`)
		r := computeApprovalReadiness(&ApprovalForm{CheckedTaskIDs: []string{"T_1", "T_3"}}, optional)
		if !r.SaveReady {
			t.Fatalf("save ready when comment optional: %+v", r)
		}
		if r.CompleteReady || len(r.MissingForComplete) != 1 {
			t.Fatalf("complete still needs path: %+v", r)
		}
	})

	t.Run("no paths no default", func(t *testing.T) {
		noPaths := approvalTestConfig(`"1"`)
		noPaths.Paths = nil
		r := computeApprovalReadiness(&ApprovalForm{Comments: "x", CheckedTaskIDs: []string{"T_1", "T_3"}}, noPaths)
		if r.DefaultSavePath != nil {
			t.Fatal("must not fabricate a default path when paths empty")
		}
		// paths 为空不增加保存必填，只影响完成（动作仍不可选）。
		if !r.SaveReady {
			t.Fatalf("save ready: %+v", r)
		}
	})

	t.Run("nil form treated as empty", func(t *testing.T) {
		r := computeApprovalReadiness(nil, cfg)
		if r.SaveReady || r.CompleteReady {
			t.Fatalf("empty form satisfies nothing: %+v", r)
		}
		if r.DefaultSavePath == nil {
			t.Fatal("default path applies to empty form")
		}
	})

	t.Run("nil config yields zero readiness", func(t *testing.T) {
		r := computeApprovalReadiness(&ApprovalForm{}, nil)
		if r.SaveReady || r.CompleteReady || len(r.MissingForSave) != 0 || len(r.MissingForComplete) != 0 {
			t.Fatalf("nil config yields zero readiness: %+v", r)
		}
	})
}

func TestParseApprovalOutput(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		out, err := parseApprovalOutput(`{"message":"正文","drafts":[]}`)
		if err != nil || out.Message != "正文" || len(out.Drafts) != 0 {
			t.Fatalf("out=%+v err=%v", out, err)
		}
	})
	t.Run("fenced", func(t *testing.T) {
		out, err := parseApprovalOutput("```json\n{\"message\":\"ok\",\"drafts\":[{\"activityId\":\"A\",\"assignmentId\":\"B\",\"form\":{\"pathId\":null,\"comments\":\"\",\"checkedTaskIds\":[]}}]}\n```")
		if err != nil || len(out.Drafts) != 1 {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		if out.Drafts[0].Form == nil || out.Drafts[0].Form.PathID != nil || out.Drafts[0].Form.Comments != "" {
			t.Fatalf("form = %+v", out.Drafts[0].Form)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		if _, err := parseApprovalOutput("not json at all"); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestAssembleApprovalDraft(t *testing.T) {
	binding := *approvalTestBinding()
	form := &ApprovalForm{PathID: strPtr("P_APPROVE"), Comments: "意见", CheckedTaskIDs: []string{"T_1", "T_1", "T_3"}}

	t.Run("new instance", func(t *testing.T) {
		d := assembleApprovalDraft(approvalModelDraft{Form: form}, binding, nil)
		if d.Revision != 1 || d.DraftID == "" || d.Status != ApprovalStatusEditing {
			t.Fatalf("draft = %+v", d)
		}
		if d.ActivityID != binding.ActivityID || d.AssignmentID != binding.AssignmentID {
			t.Fatalf("binding not stamped: %+v", d)
		}
		// 组装时去重并保证非 nil 空切片。
		if len(d.Form.CheckedTaskIDs) != 2 {
			t.Fatalf("checkedTaskIds not deduped: %v", d.Form.CheckedTaskIDs)
		}
	})

	t.Run("continues editing instance", func(t *testing.T) {
		input := &ApprovalDraft{DraftID: "draft-uuid-1", Revision: 2, Status: ApprovalStatusEditing}
		d := assembleApprovalDraft(approvalModelDraft{Form: form}, binding, input)
		if d.DraftID != "draft-uuid-1" || d.Revision != 3 {
			t.Fatalf("draft = %+v, want same id revision 3", d)
		}
	})
}

func TestApprovalOutputProblems(t *testing.T) {
	binding := approvalTestBinding()
	cfg := approvalTestConfig(`"1"`)
	if problems := approvalOutputProblems(nil, errFake(), binding, cfg); len(problems) != 1 {
		t.Fatalf("parse error problems = %v", problems)
	}
	if problems := approvalOutputProblems(&approvalModelOutput{Message: "  "}, nil, binding, cfg); len(problems) != 1 {
		t.Fatalf("empty message problems = %v", problems)
	}
	bad := &approvalModelOutput{
		Message: "正文",
		Drafts:  []approvalModelDraft{{ActivityID: "X", AssignmentID: "Y", Form: &ApprovalForm{}}},
	}
	if problems := approvalOutputProblems(bad, nil, binding, cfg); len(problems) != 1 {
		t.Fatalf("binding mismatch problems = %v", problems)
	}
	ok := &approvalModelOutput{Message: "正文", Drafts: []approvalModelDraft{{
		ActivityID: "ACT-1", AssignmentID: "ASG-1",
		Form: &ApprovalForm{PathID: strPtr("P_APPROVE"), Comments: "x", CheckedTaskIDs: []string{"T_1"}},
	}}}
	if problems := approvalOutputProblems(ok, nil, binding, cfg); len(problems) != 0 {
		t.Fatalf("valid output problems = %v", problems)
	}
}

type fakeErr struct{}

func (fakeErr) Error() string { return "fake parse error" }
func errFake() error          { return fakeErr{} }

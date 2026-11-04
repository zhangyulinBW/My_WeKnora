package aichat

import "testing"

// looksLikeAnalysisRequest 只在意图模型误判为 search 时兜底纠正，因此用例
// 覆盖两类：契约明确归 normal 的分析/图表表述（true），以及契约明确归
// search 的筛选指令和普通问句（false）。
func TestLooksLikeAnalysisRequest(t *testing.T) {
	cases := []struct {
		message string
		want    bool
	}{
		// 分析/图表类：即使意图模型误判为 search，也应纠正回 normal。
		{"统计各版本的图纸数量生成图表", true},
		{"统计各用户建立的图纸数量生成图表", true},
		{"按状态汇总一下有多少", true},
		{"画个柱状图看看金额分布", true},
		{"把当前数据做成可视化报表", true},
		{"这些订单的金额占比是多少", true},
		{"帮我求和所有未结金额", true},
		{"上个月订单的平均值是多少", true},
		{"统计未签审任务数量", true},
		{"按类型汇总一下有多少待签核任务", true},

		// 筛选/搜索类：不得被关键词误伤。
		{"帮我找出金额大于10万的单", false},
		{"查询状态为已发布的单", false},
		{"筛选上个月的所有订单", false},
		{"帮我刷新表格", false},
		{"重新整理一下表格", false},
		{"帮我找出这个月的未签审任务", false},
		{"找出所有ECO任务", false},
		{"帮我签所有ECO", false},

		// 问句/寒暄类。
		{"这个页面是做什么的？", false},
		{"你可以做什么", false},
		{"目前查询的是什么数据", false},
	}
	for _, tc := range cases {
		if got := looksLikeAnalysisRequest(tc.message); got != tc.want {
			t.Errorf("looksLikeAnalysisRequest(%q) = %v, want %v", tc.message, got, tc.want)
		}
	}
}

// TestLooksLikeApprovalActionRequest 覆盖签核动作兜底的核心边界：
// 明确任务指代/草案对象 + 签核动作 → 命中；批量查找、询问、状态统计、
// 纯状态陈述 → 不命中。
func TestLooksLikeApprovalActionRequest(t *testing.T) {
	cases := []struct {
		message string
		want    bool
	}{
		// 明确指代 + 签核动作。
		{"帮我提交这四份任务", true},
		{"帮我提交这几份任务", true},
		{"帮我提交这些任务", true},
		{"把这四份任务都提交了", true},
		{"提交这四份任务", true},
		{"这四份任务提交一下", true},
		{"帮我提交这四份任务的草案", true},
		{"帮我签这张单", true},
		{"帮我签这几份任务", true},
		{"同意这张单", true},
		{"退回这张单", true},
		{"勾选这张单的清单", true},
		{"填写这张单的意见", true},
		{"保存这张单的草案", true},
		{"提交草案", true},
		{"完成签核", true},
		{"提交当前选中的任务", true},
		{"提交已添加的四份任务", true},

		// 批量查找：仍归 approval_search，不能被本守卫改成 approval。
		{"帮我签所有ECO", false},
		{"找出所有ECO任务", false},
		{"帮我签所有任务", false},

		// 询问/解释/状态类：保持 normal。
		{"提交按钮在哪", false},
		{"这四份任务怎么提交", false},
		{"这四份任务提交后会怎样", false},
		{"这四份任务提交状态", false},
		{"这四份任务为什么要提交", false},
		{"这四份任务提交时间", false},
		{"这个节点是什么意思", false},
		{"标签是什么意思", false},
		{"统计这四份任务提交数量", false},
		{"这四份任务完成了", false},
		{"这四份任务已提交", false},
		{"这四份任务已经提交", false},
		{"这四份任务已签核", false},
		{"这四份任务已同意", false},
		{"这四份任务已退回", false},
		{"这四份任务提交过了", false},

		// 带「把…都…了」的祈使句仍应命中。
		{"把这四份任务都提交了", true},
	}
	for _, tc := range cases {
		if got := looksLikeApprovalActionRequest(tc.message); got != tc.want {
			t.Errorf("looksLikeApprovalActionRequest(%q) = %v, want %v", tc.message, got, tc.want)
		}
	}
}

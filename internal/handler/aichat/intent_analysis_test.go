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

		// 筛选/搜索类：不得被关键词误伤。
		{"帮我找出金额大于10万的单", false},
		{"查询状态为已发布的单", false},
		{"筛选上个月的所有订单", false},
		{"帮我刷新表格", false},
		{"重新整理一下表格", false},

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

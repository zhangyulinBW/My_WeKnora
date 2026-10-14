package aichat

// 本文件实现 processApproval 会话动态字段的同步：approvalTaskIndex 与
// itemData 按「字段出现整体替换、省略沿用、集合 [] 清空」的语义更新，
// 合并后的当前值落库为 role="approval_context" 消息（仿 role="start" 的
// 持久化方式），跨请求、重启后仍可回读。历史构建器只消费 user/assistant
// 角色，该消息可查询但不污染 LLM 上下文。

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

// approvalContextRole 是会话消息表中签核上下文同步消息的角色标识。
const approvalContextRole = "approval_context"

// approvalContext 是落库的同步载荷。存储前已完成合并，读取时两字段
// 总是代表当前最新同步值；字段为空表示该字段从未同步过。
type approvalContext struct {
	ApprovalTaskIndex json.RawMessage `json:"approvalTaskIndex,omitempty"`
	ItemData          json.RawMessage `json:"itemData,omitempty"`
}

// rawPayloadPresent 报告请求中的动态字段是否真的携带了载荷：
// 缺失（空）与显式 null 都按「本次未携带、沿用旧值」处理；
// 空数组 [] 是有效载荷（清空语义），不算缺失。
func rawPayloadPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// syncApprovalContext 按整体替换语义合并并落库本次请求携带的动态字段：
// 携带的字段覆盖旧值（含 [] 清空），省略的字段沿用最近一次同步值。
// 两字段都未携带时不产生新消息。幂等由消息追加语义保证：读取方总是
// 取最新一条 approval_context。
func (h *AIChatHandler) syncApprovalContext(ctx context.Context, session *types.Session, req *AIChatRequest) {
	if session == nil {
		return
	}
	// 两字段都未携带时不产生新消息，避免每轮对话重复追加相同状态。
	if !rawPayloadPresent(req.ApprovalTaskIndex) && !rawPayloadPresent(req.ItemData) {
		return
	}
	merged := mergeApprovalContext(h.loadApprovalContext(ctx, session.ID), req)
	payload, err := json.Marshal(merged)
	if err != nil {
		return
	}
	_, _ = h.messageService.CreateMessage(ctx, &types.Message{
		SessionID:   session.ID,
		Role:        approvalContextRole,
		Content:     string(payload),
		RequestID:   req.RequestID,
		IsCompleted: true,
		CreatedAt:   time.Now(),
	})
}

// mergeApprovalContext 把本次请求携带的动态字段合并进旧同步值：携带的
// 字段整体替换旧值（含 [] 清空），省略（或显式 null）的字段沿用旧值。
// 两边都无载荷时返回零值。
func mergeApprovalContext(old *approvalContext, req *AIChatRequest) approvalContext {
	merged := approvalContext{
		ApprovalTaskIndex: req.ApprovalTaskIndex,
		ItemData:          req.ItemData,
	}
	if old != nil {
		if !rawPayloadPresent(merged.ApprovalTaskIndex) {
			merged.ApprovalTaskIndex = old.ApprovalTaskIndex
		}
		if !rawPayloadPresent(merged.ItemData) {
			merged.ItemData = old.ItemData
		}
	}
	return merged
}

// loadApprovalContext 返回会话中最近一条 approval_context 消息的同步值，
// 从后往前扫描最近 50 条消息；没有同步记录时返回 nil。
func (h *AIChatHandler) loadApprovalContext(ctx context.Context, sessionID string) *approvalContext {
	msgs, err := h.messageService.GetRecentMessagesBySession(ctx, sessionID, 50)
	if err != nil {
		return nil
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m == nil || m.Role != approvalContextRole {
			continue
		}
		var payload approvalContext
		if err := json.Unmarshal([]byte(m.Content), &payload); err != nil {
			continue
		}
		return &payload
	}
	return nil
}

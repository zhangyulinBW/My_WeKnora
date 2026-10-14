package aichat

// 本文件测试 approval 意图的门控（shouldApprove）与意图注册。

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// errSessionService 让页面缺失时的会话回退静默失败（无可用会话存储）。
type errSessionService struct {
	interfaces.SessionService
}

func (e *errSessionService) GetSessionByID(_ context.Context, _ uint64, _ string) (*types.Session, error) {
	return nil, context.Canceled
}

func (e *errSessionService) CreateSession(_ context.Context, _ *types.Session) (*types.Session, error) {
	return nil, context.Canceled
}

func TestShouldApprove(t *testing.T) {
	processApprovalPage := &AIPageContext{PageType: PageTypeProcessApproval, TabID: "gotoProcessApproval"}

	cases := []struct {
		name   string
		config *config.AIChatConfig
		page   *AIPageContext
		want   bool
	}{
		{"enabled on processApproval", &config.AIChatConfig{ApprovalEnabled: true}, processApprovalPage, true},
		{"disabled by switch", &config.AIChatConfig{ApprovalEnabled: false}, processApprovalPage, false},
		{"nil config", nil, processApprovalPage, false},
		{"wrong page", &config.AIChatConfig{ApprovalEnabled: true}, &AIPageContext{PageType: "largeTable"}, false},
		{"no page in request", &config.AIChatConfig{ApprovalEnabled: true}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &AIChatHandler{
				config:         &config.Config{AIChat: c.config},
				sessionService: &errSessionService{},
			}
			req := &AIChatRequest{Page: c.page, ConversationID: "conv-should-approve"}
			// 页面缺失时 shouldApprove 会尝试会话回退；伪造服务返回错误，
			// 回退按无页面处理，与 want=false 的用例一致。
			if got := h.shouldApprove(context.Background(), req, 1); got != c.want {
				t.Fatalf("shouldApprove = %v, want %v", got, c.want)
			}
		})
	}
}

func TestApprovalIntentRegistered(t *testing.T) {
	h := &AIChatHandler{intents: newIntentRegistry()}
	h.intents.register(&normalIntentHandler{h: h})
	h.intents.register(&searchIntentHandler{h: h})
	h.intents.register(&approvalIntentHandler{h: h})
	if handler := h.intents.resolve(IntentApproval); handler == nil {
		t.Fatal("approval intent not resolvable from registry")
	}
	if handler := h.intents.resolve(Intent("unknown")); handler != nil {
		t.Fatal("unknown intent should not resolve")
	}
}

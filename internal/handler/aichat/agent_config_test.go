package aichat

// 本文件测试 ai_chat.agent 内联定义 -> 一次性 CustomAgent 的构造
// （agentFromInlineConfig）。解析优先级、tenant 盖章、默认值补齐是关键行为。

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestAgentFromInlineConfig(t *testing.T) {
	t.Run("nil when not configured", func(t *testing.T) {
		for _, cfg := range []*config.Config{nil, {}, {AIChat: &config.AIChatConfig{}}} {
			h := &AIChatHandler{config: cfg}
			if got := h.agentFromInlineConfig(context.Background()); got != nil {
				t.Fatalf("expected nil agent for config %+v, got %+v", cfg, got)
			}
		}
	})

	t.Run("synthetic id and defaults", func(t *testing.T) {
		h := &AIChatHandler{config: &config.Config{AIChat: &config.AIChatConfig{
			Agent: &config.AIChatAgentConfig{Name: "Inline"},
		}}}
		agent := h.agentFromInlineConfig(context.Background())
		if agent == nil {
			t.Fatal("expected inline agent to be built")
		}
		if agent.ID != inlineAIAgentID {
			t.Fatalf("agent.ID = %q, want synthetic %q", agent.ID, inlineAIAgentID)
		}
		if agent.Name != "Inline" {
			t.Fatalf("agent.Name = %q", agent.Name)
		}
		if agent.IsBuiltin {
			t.Fatal("inline agent must not be marked builtin")
		}
		// EnsureDefaults 已补默认值：MaxIterations=10、推荐问题配置、搜索结果数。
		if agent.Config.MaxIterations != 10 {
			t.Fatalf("MaxIterations = %d, want default 10", agent.Config.MaxIterations)
		}
		if agent.Config.QuestionSuggestions == nil {
			t.Fatal("QuestionSuggestions should be defaulted, not nil")
		}
		if agent.Config.WebSearchMaxResults != 5 {
			t.Fatalf("WebSearchMaxResults = %d, want default 5", agent.Config.WebSearchMaxResults)
		}
	})

	t.Run("explicit id honored", func(t *testing.T) {
		h := &AIChatHandler{config: &config.Config{AIChat: &config.AIChatConfig{
			Agent: &config.AIChatAgentConfig{
				ID:                 "ai-chat-main",
				CustomAgentConfig: types.CustomAgentConfig{
					AgentMode:      types.AgentModeSmartReasoning,
					SystemPrompt:   "prompt",
					ModelID:        "model-1",
					KBSelectionMode: "none",
					AllowedTools:   []string{"knowledge_search"},
				},
			},
		}}}
		agent := h.agentFromInlineConfig(context.Background())
		if agent.ID != "ai-chat-main" {
			t.Fatalf("agent.ID = %q, want ai-chat-main", agent.ID)
		}
		if agent.Config.SystemPrompt != "prompt" || agent.Config.ModelID != "model-1" {
			t.Fatalf("config not copied: %+v", agent.Config)
		}
		if len(agent.Config.AllowedTools) != 1 || agent.Config.AllowedTools[0] != "knowledge_search" {
			t.Fatalf("allowed_tools = %v", agent.Config.AllowedTools)
		}
		// smart-reasoning 强制多轮，验证内联定义同样走到 EnsureDefaults 的该分支。
		if !agent.Config.MultiTurnEnabled {
			t.Fatal("smart-reasoning inline agent should have multi-turn forced on")
		}
	})

	t.Run("tenant stamped from context", func(t *testing.T) {
		h := &AIChatHandler{config: &config.Config{AIChat: &config.AIChatConfig{
			Agent: &config.AIChatAgentConfig{},
		}}}
		ctx := types.WithExecutionTenant(context.Background(), 42)
		agent := h.agentFromInlineConfig(ctx)
		if agent.TenantID != 42 {
			t.Fatalf("agent.TenantID = %d, want 42", agent.TenantID)
		}
		// 无 tenant 上下文时不报错，交由检索侧回退会话租户。
		if bare := h.agentFromInlineConfig(context.Background()); bare == nil || bare.TenantID != 0 {
			t.Fatalf("agent without tenant ctx = %+v", bare)
		}
	})
}

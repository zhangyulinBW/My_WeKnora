package config

import (
	"strings"
	"testing"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

// unmarshalViaViper 复刻 LoadConfig 的 viper + mapstructure 解析路径
// （dc.TagName = "yaml"），但用独立 viper 实例，避免与包级全局 viper 状态互相干扰。
func unmarshalViaViper(t *testing.T, content string) *Config {
	t.Helper()
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(content)); err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg Config
	if err := v.Unmarshal(&cfg, func(dc *mapstructure.DecoderConfig) {
		dc.TagName = "yaml"
	}); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	return &cfg
}

// TestAIChatAgentInlineConfigParses 验证 ai_chat.agent 内联段的 CustomAgentConfig
// 匿名嵌入字段在 viper/mapstructure（TagName=yaml）下被扁平化解析，
// 即 model_id / system_prompt 等直接写在 agent: 下即可生效。
func TestAIChatAgentInlineConfigParses(t *testing.T) {
	cfg := unmarshalViaViper(t, `
ai_chat:
  agent:
    id: "my-inline-agent"
    name: "My Inline Agent"
    description: "defined purely in config.yaml"
    agent_mode: "smart-reasoning"
    system_prompt: "You are a helpful assistant."
    model_id: "model-1"
    kb_selection_mode: "none"
    allowed_tools:
      - "knowledge_search"
      - "grep_chunks"
    max_iterations: 15
    temperature: 0.3
  agent_id: "6a188aae-b3fb-45c7-b59b-1d0a4620a113"
`)

	if cfg.AIChat == nil {
		t.Fatal("ai_chat section missing")
	}
	agent := cfg.AIChat.Agent
	if agent == nil {
		t.Fatal("ai_chat.agent inline definition missing")
	}
	if agent.ID != "my-inline-agent" {
		t.Fatalf("agent.id = %q, want my-inline-agent", agent.ID)
	}
	if agent.Name != "My Inline Agent" || agent.Description == "" {
		t.Fatalf("agent name/description not parsed: %+v", agent)
	}
	if agent.AgentMode != "smart-reasoning" {
		t.Fatalf("agent_mode = %q", agent.AgentMode)
	}
	if agent.SystemPrompt != "You are a helpful assistant." {
		t.Fatalf("system_prompt = %q", agent.SystemPrompt)
	}
	if agent.ModelID != "model-1" {
		t.Fatalf("model_id = %q", agent.ModelID)
	}
	if agent.KBSelectionMode != "none" {
		t.Fatalf("kb_selection_mode = %q", agent.KBSelectionMode)
	}
	if len(agent.AllowedTools) != 2 || agent.AllowedTools[0] != "knowledge_search" {
		t.Fatalf("allowed_tools = %v", agent.AllowedTools)
	}
	if agent.MaxIterations != 15 || agent.Temperature != 0.3 {
		t.Fatalf("max_iterations/temperature = %d/%v", agent.MaxIterations, agent.Temperature)
	}
}

// TestAIChatAgentInlineConfigAbsent 验证未配置 agent: 段时指针为 nil，
// 保证既有 agent_id 解析链行为不变。
func TestAIChatAgentInlineConfigAbsent(t *testing.T) {
	cfg := unmarshalViaViper(t, `
ai_chat:
  agent_id: "6a188aae-b3fb-45c7-b59b-1d0a4620a113"
`)
	if cfg.AIChat == nil {
		t.Fatal("ai_chat section missing")
	}
	if cfg.AIChat.Agent != nil {
		t.Fatalf("ai_chat.agent should be nil when not configured, got %+v", cfg.AIChat.Agent)
	}
}

// TestResolveAIChatAgentPromptRefs 验证内联定义的 system_prompt_id /
// context_template_id 在启动时对照 prompt 模板回填；显式写出的内容不被覆盖，
// 模板缺失时保持为空。
func TestResolveAIChatAgentPromptRefs(t *testing.T) {
	cfg := &Config{
		AIChat: &AIChatConfig{
			Agent: &AIChatAgentConfig{ID: "inline"},
		},
		PromptTemplates: &PromptTemplatesConfig{
			AgentSystemPrompt: []PromptTemplate{{ID: "tpl-sys", Content: "SYS content"}},
			ContextTemplate:  []PromptTemplate{{ID: "tpl-ctx", Content: "CTX content"}},
		},
	}
	agent := cfg.AIChat.Agent
	agent.SystemPromptID = "tpl-sys"
	agent.ContextTemplateID = "tpl-ctx"

	resolveAIChatAgentPromptRefs(cfg)

	if agent.SystemPrompt != "SYS content" {
		t.Fatalf("system_prompt = %q, want resolved template content", agent.SystemPrompt)
	}
	if agent.ContextTemplate != "CTX content" {
		t.Fatalf("context_template = %q, want resolved template content", agent.ContextTemplate)
	}

	// 显式内容优先：ID 与内容同时存在时不覆盖。
	agent.SystemPrompt = "explicit"
	resolveAIChatAgentPromptRefs(cfg)
	if agent.SystemPrompt != "explicit" {
		t.Fatalf("explicit system_prompt overwritten: %q", agent.SystemPrompt)
	}

	// 模板缺失：保持为空，不报错。
	missing := &Config{
		AIChat: &AIChatConfig{
			Agent: &AIChatAgentConfig{},
		},
		PromptTemplates: &PromptTemplatesConfig{},
	}
	missing.AIChat.Agent.SystemPromptID = "no-such-template"
	resolveAIChatAgentPromptRefs(missing)
	if missing.AIChat.Agent.SystemPrompt != "" {
		t.Fatalf("missing template should leave system_prompt empty, got %q", missing.AIChat.Agent.SystemPrompt)
	}

	// 未配置内联定义 / 模板时为安全 no-op。
	resolveAIChatAgentPromptRefs(&Config{})
	resolveAIChatAgentPromptRefs(&Config{AIChat: &AIChatConfig{}})
}

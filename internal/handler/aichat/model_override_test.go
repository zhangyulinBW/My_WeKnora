package aichat

// 本文件测试对话模型解析优先级（resolveModelID / applyModelOverride）：
// 请求体 model_id（KnowledgeQA）> ai_chat.model_id 配置 > 智能体自身 model_id，
// 以及非法请求 model_id 的回退行为。

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// fakeModelByIDService 只覆写 GetModelByID，按 ID 返回固定模型；其余接口走
// 嵌入的空实现（未被本测试触达）。
type fakeModelByIDService struct {
	interfaces.ModelService
	models map[string]*types.Model
}

func (f *fakeModelByIDService) GetModelByID(_ context.Context, id string) (*types.Model, error) {
	return f.models[id], nil
}

const (
	testChatModelID    = "chat-model-id"
	testEmbeddingModel = "embedding-model-id"
)

func newTestHandler(configModelID string) *AIChatHandler {
	return &AIChatHandler{
		config: &config.Config{AIChat: &config.AIChatConfig{ModelID: configModelID}},
		modelService: &fakeModelByIDService{models: map[string]*types.Model{
			testChatModelID:    {ID: testChatModelID, Type: types.ModelTypeKnowledgeQA},
			testEmbeddingModel: {ID: testEmbeddingModel, Type: types.ModelTypeEmbedding},
		}},
	}
}

func testAgent(modelID string) *types.CustomAgent {
	return &types.CustomAgent{
		Config: types.CustomAgentConfig{ModelID: modelID},
	}
}

func TestResolveModelID(t *testing.T) {
	ctx := context.Background()

	t.Run("request model_id wins when KnowledgeQA", func(t *testing.T) {
		h := newTestHandler("config-model")
		req := &AIChatRequest{ModelID: testChatModelID}
		if got := h.resolveModelID(ctx, req, testAgent("agent-model")); got != testChatModelID {
			t.Fatalf("expected request model %q, got %q", testChatModelID, got)
		}
	})

	t.Run("non-KnowledgeQA request model_id falls back to config", func(t *testing.T) {
		h := newTestHandler("config-model")
		req := &AIChatRequest{ModelID: testEmbeddingModel}
		if got := h.resolveModelID(ctx, req, testAgent("agent-model")); got != "config-model" {
			t.Fatalf("expected config model %q, got %q", "config-model", got)
		}
	})

	t.Run("unknown request model_id falls back to config", func(t *testing.T) {
		h := newTestHandler("config-model")
		req := &AIChatRequest{ModelID: "does-not-exist"}
		if got := h.resolveModelID(ctx, req, testAgent("agent-model")); got != "config-model" {
			t.Fatalf("expected config model %q, got %q", "config-model", got)
		}
	})

	t.Run("empty request model_id falls back to config", func(t *testing.T) {
		h := newTestHandler("config-model")
		if got := h.resolveModelID(ctx, &AIChatRequest{}, testAgent("agent-model")); got != "config-model" {
			t.Fatalf("expected config model %q, got %q", "config-model", got)
		}
	})

	t.Run("empty config falls back to agent model", func(t *testing.T) {
		h := newTestHandler("")
		if got := h.resolveModelID(ctx, &AIChatRequest{}, testAgent("agent-model")); got != "agent-model" {
			t.Fatalf("expected agent model %q, got %q", "agent-model", got)
		}
	})
}

func TestApplyModelOverride(t *testing.T) {
	ctx := context.Background()

	t.Run("request model_id clones agent with new ModelID", func(t *testing.T) {
		h := newTestHandler("config-model")
		agent := testAgent("agent-model")
		got := h.applyModelOverride(ctx, &AIChatRequest{ModelID: testChatModelID}, agent)
		if got == agent {
			t.Fatal("expected a clone, got the original pointer")
		}
		if got.Config.ModelID != testChatModelID {
			t.Fatalf("expected ModelID %q, got %q", testChatModelID, got.Config.ModelID)
		}
		// 原对象不被写穿。
		if agent.Config.ModelID != "agent-model" {
			t.Fatalf("original agent ModelID mutated to %q", agent.Config.ModelID)
		}
	})

	t.Run("no override returns original pointer", func(t *testing.T) {
		h := newTestHandler("")
		agent := testAgent("agent-model")
		if got := h.applyModelOverride(ctx, &AIChatRequest{}, agent); got != agent {
			t.Fatal("expected original pointer when no override applies")
		}
	})
}

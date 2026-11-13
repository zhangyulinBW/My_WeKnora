package aichat

// 本文件测试签核动作兜底的完整分发链路：意图模型把
// processApproval 页面的「帮我提交这四份任务」概率性判成 normal 时，
// handleMessage 的确定性守卫应强制走 approval 并发出 approval_draft，
// 而不是落到 normal 的 answer_chunk 流。

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const guardIntentAgentID = "guard-intent-agent"

// guardAgentService 为兜底测试返回意图智能体与主智能体；其他 ID 失败，
// 让推荐问题生成走降级路径。
type guardAgentService struct {
	interfaces.CustomAgentService
	intentID    string
	intentAgent *types.CustomAgent
	mainAgent   *types.CustomAgent
}

func (f *guardAgentService) GetAgentByID(_ context.Context, id string) (*types.CustomAgent, error) {
	switch id {
	case f.intentID:
		return f.intentAgent, nil
	case defaultAIAgentID:
		return f.mainAgent, nil
	default:
		return nil, context.Canceled
	}
}

// TestHandleMessageForcesApprovalOnNormalMisclassification 覆盖用户反馈的
// 真实回归：同一条「帮我提交这四份任务」曾被意图模型判成 normal，导致
// 普通问答智能体输出 answer_chunk。这里让分类模型固定返回 normal，
// 验证守卫仍能走 approval 并发出单条 approval_draft。
func TestHandleMessageForcesApprovalOnNormalMisclassification(t *testing.T) {
	raw, err := os.ReadFile("testdata/approval_request.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var req AIChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	req.Type = AIRequestTypeMessage
	req.ConversationID = "conv-guard-normal"
	req.RequestID = "req-guard-normal"
	req.Message = "帮我提交这四份任务"

	final := `{"message":"已按提交意图生成草案。","drafts":[{"activityId":"90A5FEA6080A43CABADFF7BF76C417EA",` +
		`"assignmentId":"27D0EF7E805A445A9C1581833700ABEB",` + approvalAssessmentJSON +
		`"form":{"pathId":"9402D951D1B64F0EABD0FBA7754E9563","comments":"已填写","checkedTaskIds":["CF55C5DD589C4C98835733DC23CCD7D9"]}}]}`

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/ai/chat", nil)

	intentAgent := &types.CustomAgent{
		ID:     guardIntentAgentID,
		Config: types.CustomAgentConfig{ModelID: "intent-model", SystemPrompt: "classify"},
	}
	mainAgent := &types.CustomAgent{
		ID:     defaultAIAgentID,
		Config: types.CustomAgentConfig{ModelID: "main-model", SystemPrompt: "main"},
	}
	msgSvc := &approvalFlowMessageService{}
	sessSvc := &approvalFlowSessionService{
		session: &types.Session{ID: req.ConversationID, TenantID: 1},
		final:   final,
	}
	h := &AIChatHandler{
		config: &config.Config{AIChat: &config.AIChatConfig{
			ApprovalEnabled: true,
			IntentAgentID:   guardIntentAgentID,
		}},
		sessionService:     sessSvc,
		messageService:     msgSvc,
		customAgentService: &guardAgentService{intentID: guardIntentAgentID, intentAgent: intentAgent, mainAgent: mainAgent},
		// 意图分类与推荐问题都拿到固定 "normal"；只有 approval 分发后的
		// AgentQA 走伪会话服务，最终答案解析为合法草案。
		modelService: &fakeSuggestModelService{model: &fakeSuggestChatModel{resp: "normal"}},
		intents:      newIntentRegistry(),
	}
	h.intents.register(&normalIntentHandler{h: h})
	h.intents.register(&searchIntentHandler{h: h})
	h.intents.register(&approvalIntentHandler{h: h})
	h.intents.register(&approvalSearchIntentHandler{h: h})

	h.handleMessage(context.Background(), c, &req, mainAgent, 1)

	frames := parseSSEFrames(t, recorder.Body.String())
	var draftFrames []AIEvent
	for _, f := range frames {
		if f.Type == AIEventApprovalDraft {
			draftFrames = append(draftFrames, f)
		}
		if f.Type == AIEventAnswerChunk {
			t.Fatalf("normal answer_chunk stream leaked: %+v", f)
		}
	}
	if len(draftFrames) != 1 {
		t.Fatalf("approval_draft frames = %d, want 1 (frames: %+v)", len(draftFrames), frames)
	}
	if len(draftFrames[0].ApprovalDrafts) != 1 {
		t.Fatalf("drafts = %+v, want 1", draftFrames[0].ApprovalDrafts)
	}
	if draftFrames[0].ApprovalDrafts[0].AIAssessment == nil {
		t.Fatal("AIassessment missing from the guarded approval draft")
	}

	persistedApprovalIntent := false
	for _, m := range msgSvc.created {
		if m.Role == "intent" && strings.Contains(m.Content, `"approval"`) {
			persistedApprovalIntent = true
		}
	}
	if !persistedApprovalIntent {
		t.Fatalf("persisted intent should be approval, messages: %+v", msgSvc.created)
	}
}

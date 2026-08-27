package router

import (
	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/handler/aichat"
)

// RegisterAIRoutes registers the custom OPLink AI chat endpoint.
//
// It is a tenant-member usage surface (Viewer+) like the other chat routes, and
// a scoped API key needs the explicit "chat" capability (or full tenant access).
func RegisterAIRoutes(r *gin.RouterGroup, h *aichat.AIChatHandler, g *rbacGuards) {
	if h == nil {
		return
	}
	aiChat := g.apiKeyGroup(r.Group("/ai/chat", g.Viewer()), apiKeyChat(apiKeyFullAccess()))
	{
		aiChat.POST("", h.Chat)
	}
}

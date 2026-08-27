package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/agent/skills"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
)

type fakeUsableSkillLister struct {
	tenantID uint64
	configID string
	skills   []*types.TenantSkillEntity
}

func (f *fakeUsableSkillLister) ListUsableSkills(
	_ context.Context, tenantID uint64, configID string,
) []*types.TenantSkillEntity {
	f.tenantID = tenantID
	f.configID = configID
	if configID == "" {
		return nil
	}
	return f.skills
}

type fakePreloadedSkills struct {
	called   bool
	metadata []*skills.SkillMetadata
	err      error
}

func (f *fakePreloadedSkills) ListPreloadedSkills(
	_ context.Context,
) ([]*skills.SkillMetadata, error) {
	f.called = true
	return f.metadata, f.err
}

func (f *fakePreloadedSkills) GetSkillByName(
	_ context.Context, _ string,
) (*skills.Skill, error) {
	return nil, nil
}

func newChatSkillRouter(h *SkillHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.ErrorHandler())
	r.Use(func(c *gin.Context) {
		c.Set(types.TenantIDContextKey.String(), testSkillTenantID)
		c.Next()
	})
	r.GET("/skills", h.ListSkills)
	return r
}

func TestListSkillsHidesThePickerWhenNoSandboxConfigIsSelected(t *testing.T) {
	lister := &fakeUsableSkillLister{
		skills: []*types.TenantSkillEntity{{Name: "ppt-generator", Description: "make ppt"}},
	}
	preloaded := &fakePreloadedSkills{
		metadata: []*skills.SkillMetadata{{Name: "data-processor"}},
	}
	router := newChatSkillRouter(NewSkillHandler(lister, preloaded))

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/skills", nil))

	require.Equal(t, http.StatusOK, w.Code)
	var body struct {
		Success         bool `json:"success"`
		Data            []SkillInfoResponse
		SkillsAvailable bool `json:"skills_available"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.True(t, body.Success)
	require.Empty(t, body.Data, "preloaded and unscoped skills must not appear in @")
	require.False(t, body.SkillsAvailable)
	require.Empty(t, lister.configID)
	require.False(t, preloaded.called,
		"no selected config short-circuits before the preloaded fallback")
}

func TestListSkillsReturnsUsableInstalledSkillsForTheSelectedConfig(t *testing.T) {
	lister := &fakeUsableSkillLister{
		skills: []*types.TenantSkillEntity{
			{Name: "ppt-generator", Description: "make ppt"},
		},
	}
	preloaded := &fakePreloadedSkills{
		metadata: []*skills.SkillMetadata{{Name: "data-processor", Description: "host copy"}},
	}
	router := newChatSkillRouter(NewSkillHandler(lister, preloaded))

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "/skills?sandbox_config_id=cfg-1", nil,
	))

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, testSkillTenantID, lister.tenantID)
	require.Equal(t, "cfg-1", lister.configID)

	var body struct {
		Success         bool `json:"success"`
		Data            []SkillInfoResponse
		SkillsAvailable bool `json:"skills_available"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.True(t, body.Success)
	require.True(t, body.SkillsAvailable)
	require.Equal(t, []SkillInfoResponse{
		{Name: "ppt-generator", Description: "make ppt"},
	}, body.Data)
	require.False(t, preloaded.called,
		"the image is the source of truth whenever it carries skills")
}

// A config whose backend cannot snapshot never has an installed set, so the
// picker must show what the run will actually offer: the preloaded tree.
func TestListSkillsFallsBackToPreloadedWhenTheConfigCarriesNoImage(t *testing.T) {
	lister := &fakeUsableSkillLister{}
	preloaded := &fakePreloadedSkills{
		metadata: []*skills.SkillMetadata{
			{Name: "data-processor", Description: "分析数据"},
			nil,
			{Name: "citation-generator", Description: "生成引用"},
		},
	}
	router := newChatSkillRouter(NewSkillHandler(lister, preloaded))

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "/skills?sandbox_config_id=cfg-1", nil,
	))

	require.Equal(t, http.StatusOK, w.Code)
	var body struct {
		Success         bool `json:"success"`
		Data            []SkillInfoResponse
		SkillsAvailable bool `json:"skills_available"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.True(t, preloaded.called)
	require.True(t, body.SkillsAvailable)
	require.Equal(t, []SkillInfoResponse{
		{Name: "data-processor", Description: "分析数据"},
		{Name: "citation-generator", Description: "生成引用"},
	}, body.Data)
}

// The picker degrades to "no skills" rather than 500ing when the host tree is
// unreadable, matching what it showed before the fallback existed.
func TestListSkillsReportsNoSkillsWhenThePreloadedTreeFails(t *testing.T) {
	preloaded := &fakePreloadedSkills{err: errors.New("no such directory")}
	router := newChatSkillRouter(NewSkillHandler(&fakeUsableSkillLister{}, preloaded))

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "/skills?sandbox_config_id=cfg-1", nil,
	))

	require.Equal(t, http.StatusOK, w.Code)
	var body struct {
		Success bool `json:"success"`
		Data    []SkillInfoResponse
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.True(t, body.Success)
	require.Empty(t, body.Data)
}

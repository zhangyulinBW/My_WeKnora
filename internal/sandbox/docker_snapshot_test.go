package sandbox

import (
	"context"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/image"
	"github.com/stretchr/testify/require"
)

func TestDockerSnapshotTagAndNormalization(t *testing.T) {
	require.Equal(t, "weknora-skill:abc123", dockerSnapshotTag("abc123"))
	require.True(t, isDockerSnapshotImage("weknora-skill:abc123"))
	require.True(t, isDockerSnapshotImage("docker.io/library/weknora-skill:abc123"))
	require.False(t, isDockerSnapshotImage("wechatopenai/weknora-sandbox:latest"))
	require.False(t, isDockerSnapshotImage("weknora-skill-other:abc123"))

	require.Equal(t, "weknora-skill:abc123", mustSnapshotID(t, "weknora-skill:abc123"))
	require.Equal(t, "weknora-skill:abc123",
		mustSnapshotID(t, "docker.io/library/weknora-skill:abc123"))
	require.False(t, snapshotIDOK(t, "<none>:<none>"))
	require.False(t, snapshotIDOK(t, "weknora-skill-other:abc123"))
}

func mustSnapshotID(t *testing.T, raw string) string {
	t.Helper()
	id, ok := dockerSnapshotIDFromTag(raw)
	require.True(t, ok, "expected a snapshot ID from %q", raw)
	return id
}

func snapshotIDOK(t *testing.T, raw string) bool {
	t.Helper()
	_, ok := dockerSnapshotIDFromTag(raw)
	return ok
}

func TestDockerCreateSnapshotCommitsWithTagAndLabels(t *testing.T) {
	engine := newFakeDockerEngine()
	docker := newTestDockerClient(t, engine)

	ref, err := docker.CreateSnapshot(context.Background(), "container-1", "my-skill-image")
	require.NoError(t, err)
	require.Equal(t, "weknora-skill:container-1", ref.ID)

	require.Len(t, engine.committed, 1)
	committed := engine.committed[0]
	require.Equal(t, "weknora-skill:container-1", committed.Reference)
	require.Equal(t, "my-skill-image", committed.Comment)
	require.Contains(t, committed.Changes, "LABEL com.weknora.sandbox.snapshot=true")
	require.Contains(t, committed.Changes,
		"LABEL com.weknora.sandbox.snapshot.sandbox=container-1")
	require.Contains(t, committed.Changes,
		`LABEL com.weknora.sandbox.snapshot.name="my-skill-image"`)
}

func TestDockerCreateSnapshotRejectsEmptySandbox(t *testing.T) {
	engine := newFakeDockerEngine()
	docker := newTestDockerClient(t, engine)

	_, err := docker.CreateSnapshot(context.Background(), "  ", "")
	require.Error(t, err)
	require.Empty(t, engine.committed, "no commit should reach the daemon")
}

func TestDockerDeleteSnapshotIsIdempotent(t *testing.T) {
	engine := newFakeDockerEngine()
	docker := newTestDockerClient(t, engine)

	require.NoError(t, docker.DeleteSnapshot(context.Background(), "weknora-skill:container-1"))
	require.Equal(t, []string{"weknora-skill:container-1"}, engine.removedImages)

	engine.imageRemoveErr = cerrdefs.ErrNotFound.WithMessage("no such image")
	require.NoError(t, docker.DeleteSnapshot(context.Background(), "weknora-skill:container-1"),
		"a missing snapshot is not an error, so the prune path stays retry-safe")

	engine.imageRemoveErr = nil
	engine.removedImages = nil
	require.Error(t, docker.DeleteSnapshot(context.Background(), ""))
	require.Empty(t, engine.removedImages)
}

func TestDockerListSnapshotsFiltersAndNormalizes(t *testing.T) {
	engine := newFakeDockerEngine()
	engine.images = []image.Summary{
		{RepoTags: []string{"weknora-skill:container-1"}},
		{RepoTags: []string{"docker.io/library/weknora-skill:container-2"}},
		{RepoTags: []string{"wechatopenai/weknora-sandbox:latest"}},
		{RepoTags: []string{"weknora-skill-other:container-3"}},
		{RepoTags: []string{"<none>:<none>"}},
	}
	docker := newTestDockerClient(t, engine)

	refs, err := docker.ListSnapshots(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, refs, 2)
	require.Equal(t, "weknora-skill:container-1", refs[0].ID)
	require.Equal(t, "weknora-skill:container-2", refs[1].ID)

	require.Len(t, engine.imageListFilters, 1)
	require.True(t, engine.imageListFilters[0]["label"]["com.weknora.sandbox.snapshot=true"])
}

func TestDockerListSnapshotsScopesToSandbox(t *testing.T) {
	engine := newFakeDockerEngine()
	docker := newTestDockerClient(t, engine)

	_, err := docker.ListSnapshots(context.Background(), "container-1")
	require.NoError(t, err)
	require.Len(t, engine.imageListFilters, 1)
	require.True(t, engine.imageListFilters[0]["label"]["com.weknora.sandbox.snapshot.sandbox=container-1"])
}

// Snapshots must never surface in the admin's template picker: they are not a
// base image a workspace boots from, they are a skill payload layered on top.
func TestDockerSnapshotIsNotATemplate(t *testing.T) {
	engine := newFakeDockerEngine()
	engine.images = []image.Summary{
		{
			ID:       "sha256:snap",
			RepoTags: []string{"weknora-skill:container-1"},
			Labels:   map[string]string{"com.weknora.sandbox.snapshot": "true"},
		},
	}
	docker := newTestDockerClient(t, engine)

	templates, err := docker.ListTemplates(context.Background())
	require.NoError(t, err)
	for _, tmpl := range templates {
		require.NotEqual(t, "weknora-skill:container-1", tmpl.ID,
			"a skill snapshot must never appear in the admin's template picker")
	}
}

func TestDockerEnsureImageDoesNotPullSnapshotTags(t *testing.T) {
	engine := newFakeDockerEngine()
	docker := newTestDockerClient(t, engine)

	err := docker.ensureImage(context.Background(), "weknora-skill:container-1")
	require.Error(t, err)
	require.Empty(t, engine.pulled,
		"a local-only snapshot must not fall through to a doomed registry pull")
}

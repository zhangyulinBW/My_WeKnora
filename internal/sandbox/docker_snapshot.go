// Snapshots for the docker backend.
//
// The MicroVM providers have a snapshot endpoint that freezes a sandbox into a
// reusable template. Docker's equivalent is committing the container's
// filesystem to an image, which is enough for the one thing snapshots are used
// for here: carrying installed skills into every session the config boots.
//
// Two differences from Cube and E2B shape this file:
//
//   - A commit returns a content hash, but everything downstream treats a
//     snapshot ID as something it can boot from and list. Content-addressed
//     images have no RepoTags, so a hash would vanish from every listing. The
//     snapshot ID is therefore a tag this file generates, not the commit hash.
//   - A commit captures the filesystem and nothing else. Running processes do
//     not survive, unlike a MicroVM snapshot. Skill images are installed files,
//     so this costs nothing - but it is why these are not a general-purpose
//     "resume where you left off" snapshot.

package sandbox

import (
	"context"
	"fmt"
	"strings"

	"github.com/moby/moby/client"
)

const (
	// dockerSnapshotRepository namespaces snapshot tags.
	//
	// It must NOT be the sandbox image's repository: isStandardTemplateImage
	// compares repository paths, so tagging snapshots under
	// wechatopenai/weknora-sandbox would make every skill image show up in the
	// admin's template picker as a selectable base.
	dockerSnapshotRepository = "weknora-skill"

	// dockerSnapshotLabel marks an image as a skill snapshot. Deliberately a
	// different key from dockerTemplateLabel: the two namespaces stay disjoint,
	// so ListTemplates keeps ignoring snapshots without needing to know they
	// exist.
	dockerSnapshotLabel = "com.weknora.sandbox.snapshot"

	// dockerSnapshotSandboxLabel records the container a snapshot came from,
	// which is the only way to answer ListSnapshots(sandboxID) once the
	// container is gone.
	dockerSnapshotSandboxLabel = "com.weknora.sandbox.snapshot.sandbox"
)

var _ RemoteSnapshotManager = (*DockerRemoteClient)(nil)

// dockerSnapshotTag builds the snapshot ID for one sandbox.
//
// The container ID is the tag, not the caller's name: names are generated from
// a short config hash plus a generation counter, so two configs on one daemon
// can collide and silently overwrite each other's image. Container IDs cannot.
func dockerSnapshotTag(sandboxID string) string {
	return dockerSnapshotRepository + ":" + sandboxID
}

// isDockerSnapshotImage reports whether a reference names a skill snapshot.
func isDockerSnapshotImage(image string) bool {
	return normalizeImageRepository(image) == dockerSnapshotRepository
}

// dockerSnapshotIDFromTag maps a RepoTag the daemon reported back to the
// canonical ID this file hands out.
//
// The daemon may echo a tag fully qualified ("docker.io/library/weknora-skill:x")
// even though it was created short. Returning the daemon's spelling would make
// the reaper compare it against the stored short form, decide the two differ,
// and report a snapshot it created as an untracked extra.
func dockerSnapshotIDFromTag(raw string) (string, bool) {
	tag := strings.TrimSpace(raw)
	if tag == "" || tag == "<none>:<none>" || !isDockerSnapshotImage(tag) {
		return "", false
	}
	colon := strings.LastIndex(tag, ":")
	if colon <= strings.LastIndex(tag, "/") {
		return "", false
	}
	sandboxID := tag[colon+1:]
	if sandboxID == "" || sandboxID == "<none>" {
		return "", false
	}
	return dockerSnapshotTag(sandboxID), true
}

// CreateSnapshot commits a sandbox's filesystem to an image and returns the tag
// that image can be booted from.
func (c *DockerRemoteClient) CreateSnapshot(
	ctx context.Context, sandboxID string, name string,
) (RemoteSnapshotRef, error) {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return RemoteSnapshotRef{}, dockerInvalidRequest(
			"CreateSnapshot", "sandbox ID is required")
	}

	tag := dockerSnapshotTag(sandboxID)
	// Labels go through Changes because that is the only way ContainerCommit
	// accepts them without replacing the whole image config: passing a Config
	// would drop the entrypoint and working directory the container was made
	// with.
	changes := []string{
		fmt.Sprintf("LABEL %s=true", dockerSnapshotLabel),
		fmt.Sprintf("LABEL %s=%s", dockerSnapshotSandboxLabel, sandboxID),
	}
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		changes = append(changes, fmt.Sprintf("LABEL %s.name=%q", dockerSnapshotLabel, trimmed))
	}

	result, err := c.api.ContainerCommit(ctx, sandboxID, client.ContainerCommitOptions{
		Reference: tag,
		Comment:   strings.TrimSpace(name),
		Changes:   changes,
	})
	if err != nil {
		return RemoteSnapshotRef{}, dockerError("CreateSnapshot", err)
	}
	if strings.TrimSpace(result.ID) == "" {
		return RemoteSnapshotRef{}, dockerInvalidRequest(
			"CreateSnapshot", "daemon returned an empty image ID")
	}
	// result.ID is the content hash. The tag is what gets returned, because the
	// ID doubles as a template ID downstream and a hash is neither listable nor
	// a thing ContainerCreate should be pointed at.
	return RemoteSnapshotRef{ID: tag, Names: []string{tag}}, nil
}

// DeleteSnapshot removes a snapshot image. A missing image is not an error:
// every adapter has to treat delete as idempotent, because the prune path
// retries and the config-delete path runs after crashes.
func (c *DockerRemoteClient) DeleteSnapshot(ctx context.Context, snapshotID string) error {
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return dockerInvalidRequest("DeleteSnapshot", "snapshot ID is required")
	}
	// Force covers the window where a session that has not been rebuilt yet
	// still references a superseded image; PruneChildren reclaims the untagged
	// layers underneath, which is the bulk of the disk a stale skill image holds.
	_, err := c.api.ImageRemove(ctx, snapshotID, client.ImageRemoveOptions{
		Force:         true,
		PruneChildren: true,
	})
	if err == nil {
		return nil
	}
	wrapped := dockerError("DeleteSnapshot", err)
	if IsRemoteNotFound(wrapped) {
		return nil
	}
	return wrapped
}

// ListSnapshots reports the skill snapshots on this daemon. An empty sandboxID
// lists all of them; a non-empty one narrows to the snapshots committed from
// that container.
func (c *DockerRemoteClient) ListSnapshots(
	ctx context.Context, sandboxID string,
) ([]RemoteSnapshotRef, error) {
	filters := client.Filters{}.Add("label", dockerSnapshotLabel+"=true")
	if trimmed := strings.TrimSpace(sandboxID); trimmed != "" {
		filters = filters.Add("label", dockerSnapshotSandboxLabel+"="+trimmed)
	}
	listed, err := c.api.ImageList(ctx, client.ImageListOptions{Filters: filters})
	if err != nil {
		return nil, dockerError("ListSnapshots", err)
	}

	refs := make([]RemoteSnapshotRef, 0, len(listed.Items))
	for _, image := range listed.Items {
		for _, raw := range image.RepoTags {
			id, ok := dockerSnapshotIDFromTag(raw)
			if !ok {
				continue
			}
			refs = append(refs, RemoteSnapshotRef{ID: id, Names: []string{id}})
		}
	}
	return refs, nil
}

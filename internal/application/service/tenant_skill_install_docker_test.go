package service

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestSkillOwnerFingerprintDocker(t *testing.T) {
	docker := &types.TenantSandboxConfig{
		SandboxType: "docker",
		Docker: &types.DockerSandboxConfig{
			Host:        "unix:///var/run/docker.sock",
			TLSCertPath: "/etc/certs/client.pem",
		},
	}
	want := sandbox.SkillImageFingerprint(
		"docker", "/etc/certs/client.pem", "unix:///var/run/docker.sock")
	require.Equal(t, want, skillOwnerFingerprint(docker))

	// The host and cert are the daemon identity: a different daemon produces a
	// different owner, so its snapshot is never mistaken for ours.
	other := *docker.Docker
	other.Host = "tcp://192.0.2.5:2376"
	require.NotEqual(t, want, skillOwnerFingerprint(&types.TenantSandboxConfig{
		SandboxType: "docker", Docker: &other,
	}))
}

func TestSkillOwnerFingerprintDockerWithoutIdentity(t *testing.T) {
	// A docker config with neither host nor cert has no account to own a
	// snapshot; the install flow must abort rather than stamp a fake owner.
	require.Empty(t, skillOwnerFingerprint(&types.TenantSandboxConfig{
		SandboxType: "docker",
		Docker:      &types.DockerSandboxConfig{Image: "wechatopenai/weknora-sandbox:latest"},
	}))
	require.Empty(t, skillOwnerFingerprint(&types.TenantSandboxConfig{SandboxType: "docker"}))
}

func TestCurrentBaseTemplateDocker(t *testing.T) {
	require.Equal(t, "wechatopenai/weknora-sandbox:latest",
		currentBaseTemplate(&types.TenantSandboxConfig{
			SandboxType: "docker",
			Docker:      &types.DockerSandboxConfig{Image: "wechatopenai/weknora-sandbox:latest"},
		}))
	require.Empty(t, currentBaseTemplate(&types.TenantSandboxConfig{SandboxType: "docker"}))
}

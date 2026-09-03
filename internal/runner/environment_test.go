package runner

import (
	"testing"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
)

// TestContainerEnvironmentImplementsInterface ensures ContainerEnvironment satisfies
// the Environment interface (so future backends can be substituted).
func TestContainerEnvironmentImplementsInterface(t *testing.T) {
	t.Parallel()
	var _ Environment = (*ContainerEnvironment)(nil)
}

func TestNewContainerEnvironment(t *testing.T) {
	t.Parallel()
	m := &Manager{}
	h := host.NewHost("h", config.HostConfig{Addr: "local", OS: "linux", Arch: "amd64"})
	rc := config.RunnerConfig{
		Name:       "agentic",
		Repo:       "owner/repo",
		Host:       "h",
		Count:      2,
		Profile:    "agentic",
		RunnerMode: config.RunnerModeContainer,
	}
	env := m.NewContainerEnvironment(h, rc, 1, "agentic-2")
	if env.Kind() != config.RunnerModeContainer {
		t.Errorf("Kind() = %q, want %q", env.Kind(), config.RunnerModeContainer)
	}
	if env.instance != "agentic-2" {
		t.Errorf("instance = %q, want agentic-2", env.instance)
	}
	if env.instanceIndex != 1 {
		t.Errorf("instanceIndex = %d, want 1", env.instanceIndex)
	}
}

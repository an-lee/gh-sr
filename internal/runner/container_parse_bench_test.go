package runner

import (
	"testing"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
)

// parseContainerStatusInspectOutputInputs mirrors the inspect-output lines
// containerLocalStatusOneShot emits in production. parseContainerStatusInspectOutput
// is called once per host per Status() call, which itself runs on every TUI
// refresh tick (5s by default), so for a 10-host panel this function runs
// ~10x/refresh and is in the per-keypress render hot path indirectly.
var parseContainerStatusInspectOutputInputs = []string{
	"running|gh-sr/agentic-runner:2.320.0|sha256:abc123|deadbeef",
	"running|gh-sr/agentic-runner:2.320.0-xa1b2c3d|sha256:x|",
	"exited|gh-sr/agentic-runner:1.0.0|sha256:1|rev1",
	"restarting|x:y|sha256:4|r",
	"not installed|||",
}

// BenchmarkParseContainerStatusInspectOutput measures the per-call cost of
// parsing the pipe-delimited inspect line. strings.Cut chain is 0-alloc vs
// strings.Split's 4-element slice + padding loop.
func BenchmarkParseContainerStatusInspectOutput(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for _, in := range parseContainerStatusInspectOutputInputs {
			_, _, _ = parseContainerStatusInspectOutput(in)
		}
	}
}

// containersPresentOneShotInputs mirrors the docker ps -a --filter name=gh-sr-
// output for a typical 5-instance host. containersPresentOneShot now runs on
// every `gh sr setup` and `gh sr up` (NeedsSetup) so its amortized cost
// matters even though the SSH hop itself is dominant.
var containersPresentOneShotInputs = []string{
	"ci-1\nci-2\nci-3\nci-4\nci-5\n",
	"ci-1\nci-2\n",                  // mid-stream partial
	"\n",                            // empty / none present
	"ci-1\nother-container\nci-1\n", // host-owned name + duplicate
}

// BenchmarkContainersPresentOneShot measures the parsing cost of the batched
// container-presence helper. The mock executor's Run returns a canned list,
// so b.N iterations stay free of SSH round-trips; only the parsing path is
// on the hot loop. After the SplitSeq refactor the per-call alloc count is
// 2 (one for the result map, one for the initial `present[name]=false`
// insert that triggers bucket growth on the empty map); the previous
// `strings.Split` slice allocation is gone.
func BenchmarkContainersPresentOneShot(b *testing.B) {
	mock := &loopbackExecutor{}
	h := newContainersBenchHost(mock)
	names := []string{"ci-1", "ci-2", "ci-3", "ci-4", "ci-5"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, in := range containersPresentOneShotInputs {
			mock.set(in)
			_, _ = containersPresentOneShot(h, names)
		}
	}
}

// loopbackExecutor implements host.Executor with a swappable canned output.
// Avoids the SSH round-trip cost in benchmarks so the parsing path is the
// only thing being measured. The testutil.MockExecutor records every Call,
// which would skew alloc accounting.
type loopbackExecutor struct {
	out string
}

func (l *loopbackExecutor) Run(cmd string) (string, error) { return l.out, nil }
func (l *loopbackExecutor) Upload(localPath, remotePath string) error {
	return nil
}
func (l *loopbackExecutor) Close() error { return nil }

func (l *loopbackExecutor) set(out string) { l.out = out }

// newContainersBenchHost builds a Linux host whose Executor is the given
// loopback; mirrors the needsSetupMockHost layout so the bench sees the same
// shape as production callers.
func newContainersBenchHost(exec host.Executor) *host.Host {
	h := host.NewHost("h", config.HostConfig{Addr: "local", OS: "linux", Arch: "amd64"})
	h.SetConn(exec)
	return h
}

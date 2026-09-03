package runner

import (
	"testing"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/testutil"
)

// BenchmarkProbeDinDContainerReadiness measures the in-process parser cost of
// the combined inner-dockerd + .runner probe. The probe runs once per Status
// tick per container-mode instance, so reducing its per-call allocations flows
// directly into Manager.Status. Mock output mirrors the production "running
// + inner dockerd up + registered" case (TestProbeDinDContainerReadiness_RunningHealthy).
func BenchmarkProbeDinDContainerReadiness(b *testing.B) {
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		Output: "running\ndockerd-ok\nok\n",
	})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := ProbeDinDContainerReadiness(h, "gh-sr-x")
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkProbeDinDContainerReadiness_InnerDown mirrors the
// "running + inner dockerd down + .runner missing" case. Both probes report
// `no`, exercising the switch's default branches.
func BenchmarkProbeDinDContainerReadiness_InnerDown(b *testing.B) {
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		Output: "running\nno\nno\n",
	})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := ProbeDinDContainerReadiness(h, "gh-sr-x")
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLinuxInstanceProbe measures the parser cost of the combined
// svc.sh + systemd unit + .runner-version probe. Called once per Status
// tick per native Linux instance. Mock output mirrors the typical "svc.sh
// + user-level systemd unit + version line" case (3 markers, 3 lines).
func BenchmarkLinuxInstanceProbe(b *testing.B) {
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		Output: "S\nU\nV2.320.0\n",
	})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := linuxInstanceProbe(h, "ci-1", false)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLinuxInstanceProbe_WithDir mirrors orphan-cleanup usage, which
// passes includeDir=true and expects the leading D marker.
func BenchmarkLinuxInstanceProbe_WithDir(b *testing.B) {
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		Output: "D\nS\nU\nV2.320.0\n",
	})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := linuxInstanceProbe(h, "ci-1", true)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLinuxInstanceProbe_SystemdSystem exercises the Y (system-level
// systemd unit) branch instead of U. Same parser, different switch arm.
func BenchmarkLinuxInstanceProbe_SystemdSystem(b *testing.B) {
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		Output: "S\nY\nV2.320.0\n",
	})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := linuxInstanceProbe(h, "ci-1", false)
		if err != nil {
			b.Fatal(err)
		}
	}
}

package runner

import (
	"testing"
)

// BenchmarkContainerImageLayoutRevision measures the cost of the
// deterministic-fingerprint computation. The function sha256-sums every
// embedded container-image asset plus the gh-sr version + base image ref +
// extra-apt list and returns a 12-char hex prefix. Inputs are
// loop-invariant for a given Manager+Config (GhSrVersion,
// ContainerImageBaseImage and ContainerImageExtraApt are static during a
// single Status() invocation), so the cost is wasted when called from
// inside the per-instance loop in Manager.Status.
func BenchmarkContainerImageLayoutRevision(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = ContainerImageLayoutRevision("dev", DefaultForkRunnerImage, []string{"git", "curl", "jq"})
	}
}

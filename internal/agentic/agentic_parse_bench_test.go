package agentic

import (
	"strings"
	"testing"
)

// makeFanoutOutput returns a representative tagged stdout the way
// containerAgenticFanoutCheckCommand would emit it. Six probe blocks is the
// realistic ceiling (always-on + cache + MTU pin); the wrapper also prepends
// incidental stderr-shaped noise so the parser exercises its filter branches
// without the bench needing to construct a separate "noisy" fixture.
func makeFanoutOutput(failures int) string {
	names := []string{
		"container-inner-host-docker-internal",
		"container-node-npm",
		"container-docker-socket-user",
		"container-zstd",
		"container-cache-env",
		"container-mtu",
	}
	var b strings.Builder
	// Simulate incidental stderr-shaped noise the docker exec might let
	// through; parsers must filter these out by shape (# prefix + :N).
	b.WriteString("+ docker exec gh-sr-runner bash -lc '...'\n")
	b.WriteString("WARN: deprecated host-gateway syntax\n")
	for i, n := range names {
		tag := ":0"
		if i < failures {
			tag = ":1"
		}
		b.WriteString("#")
		b.WriteString(n)
		b.WriteString(tag)
		b.WriteByte('\n')
	}
	b.WriteString("+ cleanup tmpdir\n")
	return b.String()
}

// BenchmarkParseContainerAgenticFanoutOutput measures the per-call cost of
// the agentic fanout parser on the happy-path (zero failures) and the
// all-failed-path (6 failures) input shapes. parseContainerAgenticFanoutOutput
// is called once per `gh sr doctor` ValidateContainerAgenticFanout probe,
// which runs once per doctor invocation. The parser previously used
// strings.Split (full upfront []string allocation); switching to SplitSeq
// drops the slice allocation for the long output (the incidental noise +
// the trailing cleanup line keeps the slice longer than the typical probe
// block count).
func BenchmarkParseContainerAgenticFanoutOutput(b *testing.B) {
	specs := containerAgenticFanoutSpecs("gh-sr-runner", "agentic-1", 1400, true)
	b.Run("happy", func(b *testing.B) {
		in := makeFanoutOutput(0)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = parseContainerAgenticFanoutOutput(in, specs)
		}
	})
	b.Run("all_failed", func(b *testing.B) {
		in := makeFanoutOutput(6)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = parseContainerAgenticFanoutOutput(in, specs)
		}
	})
}

// BenchmarkParseDockerChainOutput mirrors the docker-chain parser benchmark
// (the one that powers ValidateContainerPrereqs's docker CLI / daemon /
// privileged probe trio). It also switched from strings.Split to SplitSeq
// for the same allocation reason.
func BenchmarkParseDockerChainOutput(b *testing.B) {
	specs := dockerChainSpecs("privileged")
	b.Run("clean", func(b *testing.B) {
		in := "#docker-cli:0\n#docker-daemon:0\n#docker-privileged:0\n"
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = parseDockerChainOutput(in, specs)
		}
	})
	b.Run("cli_missing", func(b *testing.B) {
		in := "#docker-cli:127\n#docker-daemon:0\n#docker-privileged:0\n"
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = parseDockerChainOutput(in, specs)
		}
	})
}

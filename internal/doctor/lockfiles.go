package doctor

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/an-lee/gh-sr/internal/runner"
)

// lockfilesLimit caps how many *.lock.yml files are fetched per repo for the
// --check-lockfiles scan (a repo can have dozens; the first 20 are plenty to
// detect a stale compile).
const lockfilesLimit = 20

// lockCompilerMinVersion is the gh-aw compiler version that emits the rootless
// (docker runtime profile, awmg-mcpg bridge gateway) artifacts gh-sr's v2
// runner image expects. Older compiles at minimum get a WARN.
const lockCompilerMinVersion = "0.88.0"

// LockFinding is one classification result for a compiled workflow file.
type LockFinding struct {
	Sev  string // sevFail or sevWarn
	Name string // workflow file name
	Msg  string
}

// AnalyzeLockWorkflow classifies one compiled gh-aw workflow's content against
// the retired sudo/iptables sandbox era. Pure function (no I/O) so the marker
// rules are unit-testable against fixture contents.
//
// FAIL markers (the compiled workflow can only run on the retired image):
//   - "sudo -E awf" — the sudo-era AWF invocation
//   - "docker-sudo-iptables" — the retired sandbox runtime profile name
//   - gateway started with --network host alongside the gh-aw-mcpg image —
//     the retired host-network gateway layout
//
// WARN: compiler_version in the front matter is older than
// lockCompilerMinVersion (compilable, but not verified against the v2 image).
func AnalyzeLockWorkflow(name, content string) []LockFinding {
	var out []LockFinding

	retired := []string{"sudo -E awf", "docker-sudo-iptables"}
	hasHostNetworkGateway := strings.Contains(content, "--network host") && strings.Contains(content, "gh-aw-mcpg")
	marker := ""
	for _, m := range retired {
		if strings.Contains(content, m) {
			marker = m
			break
		}
	}
	if marker == "" && hasHostNetworkGateway {
		marker = "--network host + gh-aw-mcpg"
	}
	if marker != "" {
		out = append(out, LockFinding{
			Sev:  sevFail,
			Name: name,
			Msg: fmt.Sprintf("compiled for the retired sudo/iptables sandbox (marker: %q); agentic jobs will fail on the v2 runner image — recompile with gh-aw >= v%s",
				marker, lockCompilerMinVersion),
		})
	}

	if v := lockCompilerVersion(content); v != "" && compareSemver(v, lockCompilerMinVersion) < 0 {
		out = append(out, LockFinding{
			Sev:  sevWarn,
			Name: name,
			Msg: fmt.Sprintf("compiler_version %s is older than v%s; recompile (gh aw compile) against gh-aw >= v%s for the rootless sandbox",
				v, lockCompilerMinVersion, lockCompilerMinVersion),
		})
	}
	return out
}

// lockCompilerVersion extracts the compiler_version from a lock.yml front
// matter; "" when absent or unparseable.
func lockCompilerVersion(content string) string {
	// SplitSeq avoids the upfront []string allocation that strings.Split makes.
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "compiler_version:") {
			continue
		}
		v := strings.TrimPrefix(line, "compiler_version:")
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		v = strings.TrimPrefix(v, "v")
		if v == "" {
			return ""
		}
		return v
	}
	return ""
}

// compareSemver compares dotted numeric versions (missing parts = 0).
func compareSemver(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		ai, bi := 0, 0
		if i < len(as) {
			ai, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bi, _ = strconv.Atoi(bs[i])
		}
		if ai != bi {
			if ai < bi {
				return -1
			}
			return 1
		}
	}
	return 0
}

// checkLockWorkflows fetches *.lock.yml for every repo in scope and reports
// legacy-era artifacts. Fetch failures downgrade to WARN so a permission
// hiccup doesn't fail the whole doctor run.
func checkLockWorkflows(w io.Writer, gh *runner.GitHubClient, repos []string, r *Result) {
	checked := 0
	for _, repo := range repos {
		files, err := gh.ListLockWorkflows(repo, lockfilesLimit)
		if err != nil {
			printLine(w, sevWarn, "lockfiles", fmt.Sprintf("repo %s: fetch failed, skipping (%v)", repo, err))
			r.Warn++
			continue
		}
		if len(files) == 0 {
			continue
		}
		needsRecompile := 0
		for _, f := range files {
			checked++
			for _, finding := range AnalyzeLockWorkflow(f.Name, f.Content) {
				if finding.Sev == sevFail {
					needsRecompile++
				}
				sev := sevWarn
				if finding.Sev == sevFail {
					sev = sevFail
				}
				printLine(w, sev, "lockfiles", fmt.Sprintf("repo %s: %s: %s", repo, finding.Name, finding.Msg))
				if sev == sevFail {
					r.Fail++
				} else {
					r.Warn++
				}
			}
		}
		if needsRecompile > 0 {
			fmt.Fprintf(w, "       Recompile: gh extension upgrade gh-aw && gh aw compile (then commit the updated .lock.yml files)\n")
		}
	}
	if checked > 0 {
		printLine(w, sevOK, "lockfiles", fmt.Sprintf("%d compiled workflow(s) scanned", checked))
	}
}

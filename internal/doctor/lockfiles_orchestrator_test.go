package doctor

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/an-lee/gh-sr/internal/runner"
)

// lockRepoGitHubStub returns an httptest server that handles the two
// ListLockWorkflows calls per repo: the directory listing at
// /repos/{owner}/{repo}/contents/.github/workflows and the per-file fetch at
// the same prefix + the file name. Other paths return 404.
//
// The `files` map is keyed by repo (e.g. "o/r") → name → contents. The
// directory listing is synthesized from the keys, and the file fetch
// base64-encodes the value (mirroring the real GitHub contents API).
// `failRepos` lists repos whose directory listing should return 502 so the
// caller exercises the fetch-failure → WARN path.
func lockRepoGitHubStub(t *testing.T, files map[string]map[string]string, failRepos map[string]bool) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		// Path shape: /repos/{owner}/{repo}/contents/.github/workflows[/{file}]
		const workflowsPrefix = "/contents/.github/workflows"
		idx := strings.Index(r.URL.Path, workflowsPrefix)
		if idx < 0 {
			http.NotFound(w, r)
			return
		}
		repo := strings.TrimPrefix(r.URL.Path[:idx], "/repos/")
		if failRepos[repo] {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		suffix := r.URL.Path[idx+len(workflowsPrefix):]
		if suffix == "" || suffix == "/" {
			// Directory listing.
			entries := []map[string]string{}
			for name := range files[repo] {
				entries = append(entries, map[string]string{"name": name, "type": "file"})
			}
			_ = json.NewEncoder(w).Encode(entries)
			return
		}
		name := strings.TrimPrefix(suffix, "/")
		content, ok := files[repo][name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"name":     name,
			"content":  base64.StdEncoding.EncodeToString([]byte(content)),
			"encoding": "base64",
		})
	}))
	t.Cleanup(ts.Close)
	return ts
}

// drainBuf consumes the rest of an io.Writer-backed buffer so the test
// never has to read it. (Unread parts do not affect correctness because the
// test asserts on the captured prefix; this is just hygiene.)
var _ = func() bool { _ = io.Discard; return true }()

// TestCheckLockWorkflows covers the orchestrator that scans every scoped
// repo for retired gh-aw lock.yml artifacts. It exercises the fetch-failure
// → WARN path, the empty-listing silent skip, the all-clean OK summary, the
// FAIL + recompile-hint path, the WARN path, and the mixed FAIL+WARN stack
// + multi-repo fan-out that the function handles across iterations.
func TestCheckLockWorkflows(t *testing.T) {
	t.Parallel()

	const rootlessLock = "name: CI\ncompiler_version: v0.88.2\non: push\n    - name: agent\n      runs-on: self-hosted\n      steps: []\n"

	t.Run("fetch failure downgrades to WARN", func(t *testing.T) {
		t.Parallel()
		srv := lockRepoGitHubStub(t, nil, map[string]bool{"o/r": true})
		gh := runner.NewGitHubClientWithHTTP("t", srv.Client(), srv.URL)

		var buf strings.Builder
		var r Result
		checkLockWorkflows(&buf, gh, []string{"o/r"}, &r)

		if r.Warn != 1 || r.Fail != 0 {
			t.Errorf("counts: got fail=%d warn=%d, want fail=0 warn=1", r.Fail, r.Warn)
		}
		if !strings.Contains(buf.String(), "WARN  [lockfiles") {
			t.Errorf("expected WARN line, got:\n%s", buf.String())
		}
		if !strings.Contains(buf.String(), "fetch failed, skipping") {
			t.Errorf("expected 'fetch failed, skipping', got:\n%s", buf.String())
		}
		if !strings.Contains(buf.String(), "repo o/r") {
			t.Errorf("expected repo name in message, got:\n%s", buf.String())
		}
		if strings.Contains(buf.String(), "scanned") {
			t.Errorf("no summary line when nothing checked, got:\n%s", buf.String())
		}
	})

	t.Run("no lock files means silent skip", func(t *testing.T) {
		t.Parallel()
		srv := lockRepoGitHubStub(t, map[string]map[string]string{"o/r": {}}, nil)
		gh := runner.NewGitHubClientWithHTTP("t", srv.Client(), srv.URL)

		var buf strings.Builder
		var r Result
		checkLockWorkflows(&buf, gh, []string{"o/r"}, &r)

		if r.Warn != 0 || r.Fail != 0 {
			t.Errorf("empty listing should not change counts, got fail=%d warn=%d", r.Fail, r.Warn)
		}
		if buf.Len() != 0 {
			t.Errorf("expected no output for empty repo, got:\n%s", buf.String())
		}
	})

	t.Run("clean lock files print OK summary only", func(t *testing.T) {
		t.Parallel()
		srv := lockRepoGitHubStub(t, map[string]map[string]string{
			"o/r": {"ci.lock.yml": rootlessLock},
		}, nil)
		gh := runner.NewGitHubClientWithHTTP("t", srv.Client(), srv.URL)

		var buf strings.Builder
		var r Result
		checkLockWorkflows(&buf, gh, []string{"o/r"}, &r)

		if r.Warn != 0 || r.Fail != 0 {
			t.Errorf("clean repo should not increment counts, got fail=%d warn=%d", r.Fail, r.Warn)
		}
		if !strings.Contains(buf.String(), "OK    [lockfiles") {
			t.Errorf("expected OK summary, got:\n%s", buf.String())
		}
		if !strings.Contains(buf.String(), "1 compiled workflow(s) scanned") {
			t.Errorf("expected scanned count, got:\n%s", buf.String())
		}
		if strings.Contains(buf.String(), "Recompile") {
			t.Errorf("clean repo must not print recompile hint, got:\n%s", buf.String())
		}
	})

	t.Run("retired sudo marker fails and prints recompile hint", func(t *testing.T) {
		t.Parallel()
		dirtyLock := strings.Replace(rootlessLock, "steps: []", "run: sudo -E awf --input-file /tmp/in", 1)
		srv := lockRepoGitHubStub(t, map[string]map[string]string{
			"o/r": {"ci.lock.yml": dirtyLock},
		}, nil)
		gh := runner.NewGitHubClientWithHTTP("t", srv.Client(), srv.URL)

		var buf strings.Builder
		var r Result
		checkLockWorkflows(&buf, gh, []string{"o/r"}, &r)

		if r.Fail != 1 || r.Warn != 0 {
			t.Errorf("FAIL should bump r.Fail, got fail=%d warn=%d", r.Fail, r.Warn)
		}
		if !strings.Contains(buf.String(), "FAIL  [lockfiles") {
			t.Errorf("expected FAIL line, got:\n%s", buf.String())
		}
		if !strings.Contains(buf.String(), `"sudo -E awf"`) {
			t.Errorf("expected quoted marker in finding, got:\n%s", buf.String())
		}
		if !strings.Contains(buf.String(), "Recompile: gh extension upgrade gh-aw") {
			t.Errorf("expected recompile hint, got:\n%s", buf.String())
		}
	})

	t.Run("old compiler version warns without recompile hint", func(t *testing.T) {
		t.Parallel()
		oldLock := strings.Replace(rootlessLock, "compiler_version: v0.88.2", "compiler_version: 0.83.1", 1)
		srv := lockRepoGitHubStub(t, map[string]map[string]string{
			"o/r": {"ci.lock.yml": oldLock},
		}, nil)
		gh := runner.NewGitHubClientWithHTTP("t", srv.Client(), srv.URL)

		var buf strings.Builder
		var r Result
		checkLockWorkflows(&buf, gh, []string{"o/r"}, &r)

		if r.Warn != 1 || r.Fail != 0 {
			t.Errorf("WARN-only finding should bump r.Warn only, got fail=%d warn=%d", r.Fail, r.Warn)
		}
		if !strings.Contains(buf.String(), "WARN  [lockfiles") {
			t.Errorf("expected WARN line, got:\n%s", buf.String())
		}
		if strings.Contains(buf.String(), "Recompile") {
			t.Errorf("WARN-only must not print recompile hint, got:\n%s", buf.String())
		}
	})

	t.Run("FAIL plus WARN stack in one file", func(t *testing.T) {
		t.Parallel()
		dirty := strings.Replace(rootlessLock, "steps: []", "run: sudo -E awf --version", 1)
		dirty = strings.Replace(dirty, "compiler_version: v0.88.2", "compiler_version: 0.85.0", 1)
		srv := lockRepoGitHubStub(t, map[string]map[string]string{
			"o/r": {"ci.lock.yml": dirty},
		}, nil)
		gh := runner.NewGitHubClientWithHTTP("t", srv.Client(), srv.URL)

		var buf strings.Builder
		var r Result
		checkLockWorkflows(&buf, gh, []string{"o/r"}, &r)

		if r.Fail != 1 || r.Warn != 1 {
			t.Errorf("FAIL+WARN should bump both counters, got fail=%d warn=%d", r.Fail, r.Warn)
		}
		if !strings.Contains(buf.String(), "Recompile:") {
			t.Errorf("any FAIL must still emit the recompile hint, got:\n%s", buf.String())
		}
		// FAIL must print before WARN (the orchestrator iterates findings in
		// the order AnalyzeLockWorkflow returns them).
		failIdx := strings.Index(buf.String(), "FAIL  [lockfiles")
		warnIdx := strings.Index(buf.String(), "WARN  [lockfiles")
		if failIdx < 0 || warnIdx < 0 || failIdx > warnIdx {
			t.Errorf("FAIL should print before WARN, got:\n%s", buf.String())
		}
	})

	t.Run("multiple repos: fetch failure + clean + FAIL aggregate", func(t *testing.T) {
		t.Parallel()
		dirtyLock := strings.Replace(rootlessLock, "steps: []", "sandbox: docker-sudo-iptables", 1)
		srv := lockRepoGitHubStub(t, map[string]map[string]string{
			"o/clean": {"ci.lock.yml": rootlessLock},
			"o/dirty": {"ci.lock.yml": dirtyLock},
		}, map[string]bool{"o/forbidden": true})
		gh := runner.NewGitHubClientWithHTTP("t", srv.Client(), srv.URL)

		var buf strings.Builder
		var r Result
		checkLockWorkflows(&buf, gh, []string{"o/forbidden", "o/clean", "o/dirty"}, &r)

		// o/forbidden: +1 Warn; o/clean: nothing; o/dirty: +1 Fail.
		if r.Fail != 1 || r.Warn != 1 {
			t.Errorf("aggregate counts: got fail=%d warn=%d, want fail=1 warn=1", r.Fail, r.Warn)
		}
		out := buf.String()
		if !strings.Contains(out, "repo o/forbidden: fetch failed") {
			t.Errorf("expected forbidden repo WARN, got:\n%s", out)
		}
		if !strings.Contains(out, "OK    [lockfiles") {
			t.Errorf("expected OK summary across repos, got:\n%s", out)
		}
		if !strings.Contains(out, "2 compiled workflow(s) scanned") {
			t.Errorf("expected total scanned = 2, got:\n%s", out)
		}
		if !strings.Contains(out, "Recompile:") {
			t.Errorf("any FAIL must emit recompile hint, got:\n%s", out)
		}
	})
}

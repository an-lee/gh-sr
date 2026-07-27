package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/an-lee/gh-sr/internal/config"
)

// TestClampCursor_branches pins the five paths of the float-to-int conversion
// guard used after every status refresh to keep the cursor on a valid row:
//
//  1. empty list (n <= 0) → 0
//  2. cursor past the end (c >= n) → n-1 (last row)
//  3. cursor before the start (c < 0) → 0
//  4. cursor already in range (0 <= c < n) → c unchanged
//  5. cursor exactly at n-1 (boundary) → n-1 unchanged
//
// A regression that drops path 1 would panic on the next index access (the
// caller assumes clampCursor guaranteed a valid index), and a regression
// in path 2 would let the cursor dangle past the last row.
func TestClampCursor_branches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		c, n int
		want int
	}{
		{"empty list clamps to 0", 5, 0, 0},
		{"negative list size clamps to 0", 0, -1, 0},
		{"cursor past end clamps to last", 10, 3, 2},
		{"cursor at exact end clamps to last", 3, 3, 2},
		{"negative cursor clamps to 0", -1, 5, 0},
		{"cursor in range unchanged", 2, 5, 2},
		{"cursor at zero unchanged", 0, 5, 0},
		{"cursor at boundary n-1 unchanged", 4, 5, 4},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := clampCursor(tc.c, tc.n); got != tc.want {
				t.Errorf("clampCursor(%d, %d) = %d, want %d", tc.c, tc.n, got, tc.want)
			}
		})
	}
}

// TestWrapLines_shortWidthDefaultsTo80 verifies the defensive guard that
// prevents wrapLines from spinning on a width < 20 (used by callers before
// the model has a real window size; without the floor the loop would
// produce a single-character-per-line dump of any non-trivial input).
func TestWrapLines_shortWidthDefaultsTo80(t *testing.T) {
	t.Parallel()
	// 100-char string. With width=10 (below floor), the function should
	// instead use 80, producing 2 lines (80 chars, then 20 chars).
	s := strings.Repeat("x", 100)
	got := wrapLines(s, 10)
	if len(got) != 2 {
		t.Errorf("wrapLines with width=10 should fall back to 80 and produce 2 lines; got %d lines: %v", len(got), got)
	}
	if len(got[0]) != 80 {
		t.Errorf("first line should be 80 chars wide, got %d", len(got[0]))
	}
}

// TestWrapLines_exactWidthOneLine documents the boundary case where the
// input is exactly the width — the loop breaks on `len(rest) > width` so
// we get one line, not two.
func TestWrapLines_exactWidthOneLine(t *testing.T) {
	t.Parallel()
	s := strings.Repeat("a", 80)
	got := wrapLines(s, 80)
	if len(got) != 1 {
		t.Errorf("wrapLines of 80-char string at width=80 should produce 1 line, got %d: %v", len(got), got)
	}
}

// TestWrapLines_widerThanWidthWraps confirms the function breaks at the
// requested width and produces the expected number of chunks.
func TestWrapLines_widerThanWidthWraps(t *testing.T) {
	t.Parallel()
	s := strings.Repeat("b", 200)
	got := wrapLines(s, 50)
	if len(got) != 4 {
		t.Errorf("wrapLines of 200-char string at width=50 should produce 4 lines, got %d: %v", len(got), got)
	}
	for i, line := range got[:3] {
		if len(line) != 50 {
			t.Errorf("line %d should be 50 chars wide, got %d", i, len(line))
		}
	}
	if len(got[3]) != 50 {
		t.Errorf("last line should be 50 chars wide, got %d", len(got[3]))
	}
}

// TestWrapLines_preservesTrailingNewline boundaries documents that wrapping
// does not split on whitespace and does not strip a trailing newline. The
// dashboard relies on this for log views where whitespace matters.
func TestWrapLines_preservesTrailingNewline(t *testing.T) {
	t.Parallel()
	// "abc\n" splits into ["abc", ""] — the empty string is the content
	// after the trailing newline. We expect wrapLines to preserve that
	// so the scroll view includes the final blank line.
	got := wrapLines("abc\n", 80)
	if len(got) != 2 || got[0] != "abc" || got[1] != "" {
		t.Errorf("wrapLines should preserve trailing newline as empty line; got %q", got)
	}
}

// TestWrapLines_emptyInputReturnsSingleEmpty documents that the empty-input
// guard returns [""] rather than [] so callers can render a blank scroll
// panel without a nil-vs-empty distinction.
func TestWrapLines_emptyInputReturnsSingleEmpty(t *testing.T) {
	t.Parallel()
	got := wrapLines("", 80)
	if len(got) != 1 || got[0] != "" {
		t.Errorf("wrapLines(\"\") should return [\"\"], got %v", got)
	}
}

// TestWrapLines_multipleNewlinesKeepsEmptyLines documents that blank lines
// between paragraphs round-trip through the wrapper. The dashboard scroll
// view relies on this to preserve log formatting.
func TestWrapLines_multipleNewlinesKeepsEmptyLines(t *testing.T) {
	t.Parallel()
	got := wrapLines("a\n\nb", 80)
	want := []string{"a", "", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("wrapLines(\"a\\n\\nb\") = %v, want %v", got, want)
	}
}

// TestSortedHostNames_returnsAlphabeticalOrder pins the contract that the
// filter host picker shows hosts in a stable alphabetical order regardless
// of map iteration order. Without the sort, the picker would shuffle on
// every refresh.
func TestSortedHostNames_returnsAlphabeticalOrder(t *testing.T) {
	t.Parallel()
	m := &dashboardModel{
		cfg: &config.Config{
			Hosts: map[string]config.HostConfig{
				"zulu":    {Addr: "z"},
				"alpha":   {Addr: "a"},
				"mike":    {Addr: "m"},
				"bravo":   {Addr: "b"},
				"nothing": {},
			},
		},
	}
	got := m.sortedHostNames()
	want := []string{"alpha", "bravo", "mike", "nothing", "zulu"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sortedHostNames = %v, want %v", got, want)
	}
}

// TestSortedHostNames_emptyMapReturnsEmptySlice pins empty-input behavior
// so the filter panel can render "(no values)" without a nil-vs-empty
// distinction.
func TestSortedHostNames_emptyMapReturnsEmptySlice(t *testing.T) {
	t.Parallel()
	m := &dashboardModel{cfg: &config.Config{Hosts: map[string]config.HostConfig{}}}
	got := m.sortedHostNames()
	if len(got) != 0 {
		t.Errorf("sortedHostNames on empty hosts = %v, want empty slice", got)
	}
}

// TestSortedRepoNames_orgPrefix pins the "org:" prefix applied to runners
// scoped to an organization. The filter picker uses this prefix to
// distinguish org-scoped runners from repo-scoped ones in the same list.
func TestSortedRepoNames_orgPrefix(t *testing.T) {
	t.Parallel()
	m := &dashboardModel{
		cfg: &config.Config{
			Runners: []config.RunnerConfig{
				{Org: "my-org", Name: "r1"},
				{Org: "my-org", Name: "r2"},
				{Repo: "owner/repo", Name: "r3"},
			},
		},
	}
	got := m.sortedRepoNames()
	want := []string{"org:my-org", "owner/repo"}
	// Note: the "org:my-org" appears once because the dedup is on the
	// scope string, not the runner name. Two runners sharing the same
	// "my-org" scope collapse into a single filter entry.
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sortedRepoNames = %v, want %v", got, want)
	}
}

// TestSortedRepoNames_orgTakesPriorityOverRepo pins the rule that if a
// runner has Org set, the repo filter is ignored — the runner is scoped
// to the org, not to an individual repo. Mixing Org and Repo on the same
// runner struct is a configuration error, but the picker has to pick one
// and the current rule favors Org.
func TestSortedRepoNames_orgTakesPriorityOverRepo(t *testing.T) {
	t.Parallel()
	m := &dashboardModel{
		cfg: &config.Config{
			Runners: []config.RunnerConfig{
				{Org: "my-org", Repo: "should-be-ignored", Name: "r1"},
			},
		},
	}
	got := m.sortedRepoNames()
	want := []string{"org:my-org"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sortedRepoNames with Org set should ignore Repo; got %v, want %v", got, want)
	}
}

// TestSortedRepoNames_neitherOrgNorRepoAddsEmptyString pins the current
// behaviour where a runner with neither Org nor Repo contributes an empty
// string to the filter list. The empty string sorts first because it is
// lexicographically smaller than every non-empty value. This is a
// pre-existing edge case in the picker (the entry would render as a blank
// line in the menu but cannot match any real filter); documenting it here
// so a future fix to skip scope-less runners is flagged as a behaviour
// change rather than a silent regression.
func TestSortedRepoNames_neitherOrgNorRepoAddsEmptyString(t *testing.T) {
	t.Parallel()
	m := &dashboardModel{
		cfg: &config.Config{
			Runners: []config.RunnerConfig{
				{Org: "my-org", Name: "r1"},
				{Name: "r2"}, // no scope — current code adds seen[""] = true
				{Repo: "owner/repo", Name: "r3"},
			},
		},
	}
	got := m.sortedRepoNames()
	// Empty string sorts first, then "org:my-org", then "owner/repo".
	want := []string{"", "org:my-org", "owner/repo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sortedRepoNames = %v, want %v (current behaviour includes \"\" for scope-less runners)", got, want)
	}
}

// TestEnsureEnvFile_createsMissingFile pins the "first run" path: when the
// env file does not exist, ensureEnvFile creates the parent directory
// (0o700) and writes the template (0o600). The dashboard relies on this
// when the user picks "Edit env file" from the global menu before the
// env file has been bootstrapped.
func TestEnsureEnvFile_createsMissingFile(t *testing.T) {
	// No t.Parallel(): touches a per-test tempdir.
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "env")
	if err := ensureEnvFile(path); err != nil {
		t.Fatalf("ensureEnvFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("env file not created: %v", err)
	}
	if info.Size() == 0 {
		t.Errorf("env file is empty; should contain EnvFileTemplate")
	}
	// Permissions: 0o600 (owner read/write only).
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("env file perm = %o, want 0o600", perm)
	}
	// Parent directory: 0o700 (owner full access only).
	parentInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("parent dir not created: %v", err)
	}
	if perm := parentInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("parent dir perm = %o, want 0o700", perm)
	}
}

// TestEnsureEnvFile_preservesExistingFile pins the "not first run" path:
// when the env file already exists, ensureEnvFile returns nil and leaves
// the file untouched. Existing user's edits must be preserved across
// dashboard re-opens.
func TestEnsureEnvFile_preservesExistingFile(t *testing.T) {
	// No t.Parallel(): touches a per-test tempdir.
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	original := "GITHUB_TOKEN=secret\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ensureEnvFile(path); err != nil {
		t.Fatalf("ensureEnvFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after ensureEnvFile: %v", err)
	}
	if string(got) != original {
		t.Errorf("ensureEnvFile should not overwrite existing file; got %q, want %q", got, original)
	}
}

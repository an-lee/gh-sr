package hostshell

import (
	"errors"
	"strings"
	"testing"
)

func TestPosixSingleQuote(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  string
	}{
		// Empty string produces empty single-quoted string.
		{"empty", "", "''"},
		// Simple strings pass through unchanged inside quotes.
		{"simple", "hello", "'hello'"},
		{"no_spaces", "no spaces", "'no spaces'"},
		{"path", "path/to/file", "'path/to/file'"},
		// Single quotes are escaped via the POSIX idiom ' → '\''.
		{"apos_one", "it's", "'it'\\''s'"},
		{"apos_multi", "a'b'c", "'a'\\''b'\\''c'"},
		// Two consecutive single quotes.
		{"two_apos", "''", "''\\'''\\'''"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := PosixSingleQuote(tc.input)
			if got != tc.want {
				t.Errorf("PosixSingleQuote(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestPowerShellSingleQuote(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"simple", "hello", "'hello'"},
		{"single_apos", "didn't", "'didn''t'"},
		{"double_quote_inside", `it's "quoted"`, `'it''s "quoted"'`},
		{"apos_between", "a'b", "'a''b'"},
		{"empty", "", "''"},
		{"no_quotes", "no quotes", "'no quotes'"},
		{"trailing_apos", "trailing'", "'trailing'''"},
		{"leading_apos", "'leading'", "'''leading'''"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := PowerShellSingleQuote(tc.input)
			if got != tc.want {
				t.Errorf("PowerShellSingleQuote(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestPlistEscape(t *testing.T) {
	t.Parallel()
	got := PlistEscape(`a&b"c<d>e`)
	want := `a&amp;b&quot;c&lt;d&gt;e`
	if got != want {
		t.Fatalf("PlistEscape = %q, want %q", got, want)
	}
}

func TestLinuxElevatePrelude(t *testing.T) {
	t.Parallel()
	// Both callers produce shells that:
	//   * initialise $SUDO to ''
	//   * detect root via id -u
	//   * try passwordless sudo (sudo -n true)
	//   * print the supplied failureMsg and exit 1 on failure
	// The literal failureMsg is inlined into the script via PosixSingleQuote
	// so we assert on its presence and the surrounding shell skeleton.
	cases := []struct {
		name       string
		failureMsg string
		wantSubs   []string
	}{
		{
			name:       "runner_message",
			failureMsg: "gh sr: remote Linux commands need root SSH or passwordless sudo (non-interactive); SSH has no TTY for sudo passwords. Use NOPASSWD, connect as root, or install software manually. Run: gh sr doctor",
			wantSubs: []string{
				`SUDO=''`,
				`if [ "$(id -u)" -ne 0 ]`,
				`command -v sudo >/dev/null 2>&1`,
				`sudo -n true 2>/dev/null`,
				`SUDO='sudo -n'`,
				`gh sr: remote Linux commands`,
				`exit 1`,
			},
		},
		{
			name:       "autostart_message",
			failureMsg: "gh sr: system-level autostart needs root SSH or passwordless sudo (non-interactive)",
			wantSubs: []string{
				`SUDO=''`,
				`if [ "$(id -u)" -ne 0 ]`,
				`command -v sudo >/dev/null 2>&1`,
				`sudo -n true 2>/dev/null`,
				`SUDO='sudo -n'`,
				`gh sr: system-level autostart`,
				`exit 1`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := LinuxElevatePrelude(tc.failureMsg)
			for _, sub := range tc.wantSubs {
				if !strings.Contains(got, sub) {
					t.Errorf("LinuxElevatePrelude missing substring %q in output:\n%s", sub, got)
				}
			}
		})
	}
}

func TestLinuxElevatePreludeSoft(t *testing.T) {
	t.Parallel()
	// The soft variant must:
	//   * initialise $SUDO to ''
	//   * detect root via id -u
	//   * try passwordless sudo (sudo -n true)
	//   * set SUDO='sudo -n' on success
	// and must NOT:
	//   * print anything to stderr
	//   * exit (callers gate usage of $SUDO themselves)
	got := LinuxElevatePreludeSoft()
	wantSubs := []string{
		`SUDO=''`,
		`if [ "$(id -u)" -ne 0 ]`,
		`command -v sudo >/dev/null 2>&1`,
		`sudo -n true 2>/dev/null`,
		`SUDO='sudo -n'`,
	}
	for _, sub := range wantSubs {
		if !strings.Contains(got, sub) {
			t.Errorf("LinuxElevatePreludeSoft missing substring %q in output:\n%s", sub, got)
		}
	}
	// Guard against accidental drift toward the strict variant.
	for _, banned := range []string{"exit 1", ">&2", `echo `} {
		if strings.Contains(got, banned) {
			t.Errorf("LinuxElevatePreludeSoft must not contain %q (strict-variant behaviour) but got:\n%s", banned, got)
		}
	}
}

func TestPowerShellBoolCheck(t *testing.T) {
	t.Parallel()
	t.Run("yes output returns true", func(t *testing.T) {
		t.Parallel()
		var gotScript string
		runner := func(script string) (string, error) {
			gotScript = script
			return "yes\n", nil
		}
		ok, err := PowerShellBoolCheck(runner, "Get-Something -Name 'foo'")
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatal("expected true")
		}
		if !strings.Contains(gotScript, "if (") || !strings.Contains(gotScript, "Get-Something") {
			t.Fatalf("script = %q; want wrapped condition", gotScript)
		}
	})
	t.Run("no output returns false", func(t *testing.T) {
		t.Parallel()
		runner := func(string) (string, error) { return "no\r\n", nil }
		ok, err := PowerShellBoolCheck(runner, "cond")
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatal("expected false")
		}
	})
	t.Run("error propagates", func(t *testing.T) {
		t.Parallel()
		runner := func(string) (string, error) { return "", errors.New("boom") }
		_, err := PowerShellBoolCheck(runner, "cond")
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestScheduledTaskExists(t *testing.T) {
	t.Parallel()
	t.Run("task present", func(t *testing.T) {
		t.Parallel()
		var gotScript string
		runner := func(script string) (string, error) {
			gotScript = script
			return "yes", nil
		}
		ok, err := ScheduledTaskExists(runner, "my-task")
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatal("expected true")
		}
		if !strings.Contains(gotScript, "Get-ScheduledTask") || !strings.Contains(gotScript, "-TaskName") {
			t.Fatalf("script = %q; want Get-ScheduledTask probe", gotScript)
		}
		// Name must be single-quoted (PowerShellSingleQuote).
		if !strings.Contains(gotScript, "'my-task'") {
			t.Fatalf("script = %q; name not properly quoted", gotScript)
		}
	})
	t.Run("task absent", func(t *testing.T) {
		t.Parallel()
		runner := func(string) (string, error) { return "no", nil }
		ok, err := ScheduledTaskExists(runner, "gone")
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatal("expected false")
		}
	})
	t.Run("apostrophe in name is safely quoted", func(t *testing.T) {
		t.Parallel()
		var gotScript string
		runner := func(script string) (string, error) {
			gotScript = script
			return "yes", nil
		}
		_, err := ScheduledTaskExists(runner, "O'Brien")
		if err != nil {
			t.Fatal(err)
		}
		// PowerShellSingleQuote doubles apostrophes: O'Brien → 'O''Brien'
		if !strings.Contains(gotScript, "'O''Brien'") {
			t.Fatalf("script = %q; apostrophe not safely escaped", gotScript)
		}
	})
	t.Run("error propagates", func(t *testing.T) {
		t.Parallel()
		runner := func(string) (string, error) { return "", errors.New("ps failed") }
		_, err := ScheduledTaskExists(runner, "task")
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestScheduledTaskState(t *testing.T) {
	t.Parallel()
	t.Run("returns trimmed state", func(t *testing.T) {
		t.Parallel()
		var gotScript string
		runner := func(script string) (string, error) {
			gotScript = script
			return "Ready\r\n", nil
		}
		state, err := ScheduledTaskState(runner, "my-task")
		if err != nil {
			t.Fatal(err)
		}
		if state != "Ready" {
			t.Fatalf("state = %q; want Ready", state)
		}
		if !strings.Contains(gotScript, "Select-Object -ExpandProperty State") {
			t.Fatalf("script = %q; missing state extraction", gotScript)
		}
		if !strings.Contains(gotScript, "'my-task'") {
			t.Fatalf("script = %q; name not properly quoted", gotScript)
		}
	})
	t.Run("error propagates", func(t *testing.T) {
		t.Parallel()
		runner := func(string) (string, error) { return "", errors.New("state failed") }
		_, err := ScheduledTaskState(runner, "task")
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

package ps

import (
	"os/exec"
	"reflect"
	"runtime"
	"testing"
)

func TestCommandArgs(t *testing.T) {
	t.Parallel()
	got := CommandArgs("echo hi")
	want := []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "echo hi"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CommandArgs() = %v, want %v", got, want)
	}
}

func TestCommandLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		script string
		want   string
	}{
		{
			script: "[Environment]::OSVersion.Platform",
			want:   `powershell.exe -NoProfile -NonInteractive -Command "[Environment]::OSVersion.Platform"`,
		},
		{
			script: "$env:PROCESSOR_ARCHITECTURE",
			want:   `powershell.exe -NoProfile -NonInteractive -Command "$env:PROCESSOR_ARCHITECTURE"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.script, func(t *testing.T) {
			t.Parallel()
			if got := CommandLine(tc.script); got != tc.want {
				t.Fatalf("CommandLine(%q) = %q, want %q", tc.script, got, tc.want)
			}
		})
	}
}

// fakeCmd replaces runCmd for the duration of a single test. It records the
// argv the production code passed and returns a real *exec.Cmd pointing at
// a shell command that exits 0 and prints canned stdout/stderr. This lets
// Exec and CombinedOutput run end-to-end on every test host without needing
// powershell.exe on $PATH.
//
// The recorded argv is compared against CommandArgs output in each test that
// uses fakeCmd — verifying that Exec/CombinedOutput wire the script through
// CommandArgs exactly as documented.
type fakeCmd struct {
	args []string
}

func (f *fakeCmd) last() []string { return append([]string(nil), f.args...) }

// shellCanned returns an *exec.Cmd that, when run, prints `stdout` to stdout,
// `stderr` to stderr, and exits 0. Used so Exec/CombinedOutput can complete
// their .Output()/.CombinedOutput() calls deterministically on the test host.
func shellCanned(stdout, stderr string) *exec.Cmd {
	switch runtime.GOOS {
	case "windows":
		// cmd /c prints each line followed by a CRLF. CombinedOutput joins
		// stdout+stderr; Output only takes stdout. We split them with goto
		// so the two streams land in the right pipes.
		script := ""
		if stdout != "" {
			script += "echo " + stdout + "&"
		}
		if stderr != "" {
			script += "echo " + stderr + " 1>&2&"
		}
		script += "exit /b 0"
		return exec.Command("cmd", "/c", script)
	default:
		script := ""
		if stdout != "" {
			script += "printf '%s' " + stdout + ";"
		}
		if stderr != "" {
			script += "printf '%s' " + stderr + " 1>&2;"
		}
		return exec.Command("sh", "-c", script)
	}
}

// withFakeRunCmd swaps the package-level runCmd for the duration of the test
// and returns a *fakeCmd that records the argv of the first invocation, plus
// a cleanup function that restores runCmd. argv matching is single-shot
// because each test exercises exactly one Exec / CombinedOutput call.
func withFakeRunCmd(t *testing.T, stdout, stderr string) (*fakeCmd, func()) {
	t.Helper()
	prev := runCmd
	fc := &fakeCmd{}
	runCmd = func(name string, args ...string) *exec.Cmd {
		fc.args = append([]string{name}, args...)
		return shellCanned(stdout, stderr)
	}
	return fc, func() { runCmd = prev }
}

func TestExec_passesArgsViaRunCmd(t *testing.T) {
	fc, restore := withFakeRunCmd(t, "hello", "")
	defer restore()

	out, err := Exec("Get-Date")
	if err != nil {
		t.Fatalf("Exec() error = %v, want nil", err)
	}
	if string(out) != "hello" {
		t.Errorf("Exec() stdout = %q, want %q", out, "hello")
	}
	want := CommandArgs("Get-Date")
	if !reflect.DeepEqual(fc.last(), want) {
		t.Errorf("Exec() argv = %v, want %v", fc.last(), want)
	}
}

func TestCombinedOutput_mergesStdoutStderr(t *testing.T) {
	fc, restore := withFakeRunCmd(t, "ok", "warn")
	defer restore()

	out, err := CombinedOutput("Get-Process")
	if err != nil {
		t.Fatalf("CombinedOutput() error = %v, want nil", err)
	}
	want := "okwarn"
	if string(out) != want {
		t.Errorf("CombinedOutput() stdout+stderr = %q, want %q", out, want)
	}
	wantArgs := CommandArgs("Get-Process")
	if !reflect.DeepEqual(fc.last(), wantArgs) {
		t.Errorf("CombinedOutput() argv = %v, want %v", fc.last(), wantArgs)
	}
}

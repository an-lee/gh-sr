package host

import (
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/testutil"
)

func Test_normalizeArch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input   string
		want    string
		wantErr bool
	}{
		// Valid amd64 inputs
		{"x86_64", "amd64", false},
		{"amd64", "amd64", false},
		{"X86_64", "amd64", false},
		{"AMD64", "amd64", false},
		// Valid arm64 inputs
		{"aarch64", "arm64", false},
		{"arm64", "arm64", false},
		{"AARCH64", "arm64", false},
		{"ARM64", "arm64", false},
		// Error cases
		{"i386", "", true},
		{"UNKNOWN", "", true},
		{"", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeArch(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("normalizeArch(%q): expected error, got %q", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Errorf("normalizeArch(%q): unexpected error: %v", tc.input, err)
				return
			}
			if got != tc.want {
				t.Errorf("normalizeArch(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func Test_parseAddr(t *testing.T) {
	t.Parallel()
	u, a := parseAddr("user@host.example:2222")
	if u != "user" || a != "host.example:2222" {
		t.Fatalf("with user: got %q %q", u, a)
	}
	u, a = parseAddr("192.168.1.1")
	if u != "" || a != "192.168.1.1" {
		t.Fatalf("no user: got %q %q", u, a)
	}
}

func TestHost_SSHUser(t *testing.T) {
	t.Parallel()
	cases := []struct {
		addr string
		want string
	}{
		{"an-lee@192.168.31.66", "an-lee"},
		{"user@host.example:2222", "user"},
		{"192.168.1.1", ""},
		{"local", ""},
		{"Local", ""},
	}
	for _, tc := range cases {
		h := NewHost("h", config.HostConfig{Addr: tc.addr, OS: "linux", Arch: "amd64"})
		if got := h.SSHUser(); got != tc.want {
			t.Errorf("SSHUser(%q): got %q want %q", tc.addr, got, tc.want)
		}
	}
}

func TestEncodePowerShellScript_roundTrip(t *testing.T) {
	t.Parallel()
	script := "Write-Host \"a\"'\nline2"
	enc := encodePowerShellScript(script)
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw)%2 != 0 {
		t.Fatalf("UTF-16LE byte length should be even, got %d", len(raw))
	}
	u16 := make([]uint16, len(raw)/2)
	for i := range u16 {
		u16[i] = uint16(raw[i*2]) | uint16(raw[i*2+1])<<8
	}
	got := string(utf16.Decode(u16))
	if got != script {
		t.Errorf("round-trip: got %q want %q", got, script)
	}
}

func TestHost_wrapCommand(t *testing.T) {
	t.Parallel()
	h := NewHost("w", config.HostConfig{Addr: "u@h", OS: "windows", Arch: "amd64"})
	got := h.wrapCommand(`Write-Host "ok"`)
	if !strings.Contains(got, "powershell.exe") {
		t.Fatalf("windows default exe: %q", got)
	}
	if !strings.Contains(got, "-EncodedCommand") {
		t.Fatalf("should use EncodedCommand: %q", got)
	}
	if strings.Contains(got, "-Command") {
		t.Fatalf("should not use -Command (quoting): %q", got)
	}

	hPW := NewHost("w", config.HostConfig{Addr: "u@h", OS: "windows", Arch: "amd64", WindowsPS: "pwsh"})
	gotPW := hPW.wrapCommand(`1`)
	if !strings.Contains(gotPW, "pwsh.exe") {
		t.Fatalf("windows_ps pwsh: %q", gotPW)
	}

	ln := NewHost("l", config.HostConfig{Addr: "u@h", OS: "linux", Arch: "amd64"})
	if ln.wrapCommand("echo hi") != "echo hi" {
		t.Fatalf("linux should pass through")
	}
}

func TestIsLocal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		addr string
		want bool
	}{
		{"local", true},
		{"Local", true},
		{"LOCAL", true},
		{" local ", true},
		{"user@host", false},
		{"", false},
		{"localhost", false},
	}
	for _, tc := range cases {
		if got := IsLocal(tc.addr); got != tc.want {
			t.Errorf("IsLocal(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestHost_ConnectLocal(t *testing.T) {
	t.Parallel()
	h := NewHost("local-box", config.HostConfig{Addr: "local", OS: "linux", Arch: "amd64"})
	if err := h.Connect(); err != nil {
		t.Fatalf("Connect local: %v", err)
	}
	defer h.Close()

	if _, ok := h.conn.(*LocalConnection); !ok {
		t.Fatalf("expected *LocalConnection, got %T", h.conn)
	}

	out, err := h.Run("echo works")
	if err != nil {
		t.Fatalf("Run on local: %v", err)
	}
	if out != "works" {
		t.Errorf("got %q, want %q", out, "works")
	}
}

func TestHost_RunConcurrentLazyConnect(t *testing.T) {
	t.Parallel()

	h := NewHost("local-box", config.HostConfig{Addr: "local", OS: "linux", Arch: "amd64"})
	defer h.Close()

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := h.Run("printf ok")
			if err != nil {
				t.Errorf("Run on local: %v", err)
				return
			}
			if out != "ok" {
				t.Errorf("got %q, want %q", out, "ok")
			}
		}()
	}
	wg.Wait()
}

func TestHost_wrapCommand_localWindows(t *testing.T) {
	t.Parallel()
	h := NewHost("local-win", config.HostConfig{Addr: "local", OS: "windows", Arch: "amd64"})
	got := h.wrapCommand("Get-Date")
	if got != "Get-Date" {
		t.Errorf("local windows should pass through, got %q", got)
	}
}

func TestHost_paths(t *testing.T) {
	t.Parallel()
	win := NewHost("w", config.HostConfig{Addr: "u@h", OS: "windows", Arch: "amd64"})
	if win.RunnerBaseDir() != `$env:USERPROFILE\.gh-sr\runners` {
		t.Errorf("windows base: %q", win.RunnerBaseDir())
	}
	if win.RunnerBaseDirPS() != `Join-Path $env:USERPROFILE '.gh-sr\runners'` {
		t.Errorf("windows base ps: %q", win.RunnerBaseDirPS())
	}
	if win.RunnerDir("r1") != `$env:USERPROFILE\.gh-sr\runners\r1` {
		t.Errorf("windows runner dir: %q", win.RunnerDir("r1"))
	}
	if win.RunnerDirPS("r1") != `Join-Path (Join-Path $env:USERPROFILE '.gh-sr\runners') 'r1'` {
		t.Errorf("windows runner dir ps: %q", win.RunnerDirPS("r1"))
	}
	if win.TempDir() != "$env:TEMP" {
		t.Errorf("windows temp: %q", win.TempDir())
	}
	if win.TempDirPS() != "$env:TEMP" {
		t.Errorf("windows temp ps: %q", win.TempDirPS())
	}
	if win.PathSep() != `\` {
		t.Errorf("windows sep: %q", win.PathSep())
	}

	ln := NewHost("l", config.HostConfig{Addr: "u@h", OS: "linux", Arch: "arm64"})
	if ln.RunnerBaseDir() != "$HOME/.gh-sr/runners" {
		t.Errorf("linux base: %q", ln.RunnerBaseDir())
	}
	if ln.RunnerBaseDirPS() != "$HOME/.gh-sr/runners" {
		t.Errorf("linux base ps: %q", ln.RunnerBaseDirPS())
	}
	if ln.RunnerDir("r1") != "$HOME/.gh-sr/runners/r1" {
		t.Errorf("linux runner dir: %q", ln.RunnerDir("r1"))
	}
	if ln.RunnerDirPS("r1") != "$HOME/.gh-sr/runners/r1" {
		t.Errorf("linux runner dir ps: %q", ln.RunnerDirPS("r1"))
	}
	if ln.TempDir() != "/tmp" {
		t.Errorf("linux temp: %q", ln.TempDir())
	}
	if ln.TempDirPS() != "/tmp" {
		t.Errorf("linux temp ps: %q", ln.TempDirPS())
	}
	if ln.PathSep() != "/" {
		t.Errorf("linux sep: %q", ln.PathSep())
	}
}

// TestHost_Upload_delegatesToConn covers the Host.Upload wrapper: the
// injected Executor must observe the (localPath, remotePath) pair and
// any error it returns must surface unchanged. SetConn is used to skip
// the SSH dial path so the test stays hermetic.
func TestHost_Upload_delegatesToConn(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{}
	h := NewHost("h", config.HostConfig{Addr: "u@h", OS: "linux", Arch: "amd64"})
	h.SetConn(mock)

	if err := h.Upload("/local/src.txt", "/remote/dst.txt"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !mock.UploadCalled {
		t.Fatal("Executor.Upload was not invoked")
	}
	if mock.LastUpload.Local != "/local/src.txt" || mock.LastUpload.Remote != "/remote/dst.txt" {
		t.Errorf("uploaded paths: got (%q, %q), want (/local/src.txt, /remote/dst.txt)",
			mock.LastUpload.Local, mock.LastUpload.Remote)
	}
}

// TestHost_Upload_propagatesError covers the error-surfacing branch:
// the Executor.Upload error must reach the Host.Upload caller verbatim
// so call sites can match on errors.Is for retry/deadline decisions.
func TestHost_Upload_propagatesError(t *testing.T) {
	t.Parallel()
	want := errors.New("scp timed out")
	mock := &testutil.MockExecutor{UploadErr: want}
	h := NewHost("h", config.HostConfig{Addr: "u@h", OS: "linux", Arch: "amd64"})
	h.SetConn(mock)

	if err := h.Upload("/local/src.txt", "/remote/dst.txt"); !errors.Is(err, want) {
		t.Fatalf("Upload error: got %v, want wraps %v", err, want)
	}
}

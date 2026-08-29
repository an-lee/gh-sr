package host

import (
	"strings"
	"testing"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/testutil"
)

func TestDetectOS(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  config.HostConfig
		mock *testutil.MockExecutor
		want string
		err  bool
	}{
		{
			name: "linux",
			cfg:  config.HostConfig{OS: "linux"},
			mock: &testutil.MockExecutor{Output: "Linux"},
			want: "linux",
		},
		{
			name: "darwin",
			cfg:  config.HostConfig{OS: "linux"},
			mock: &testutil.MockExecutor{Output: "Darwin"},
			want: "darwin",
		},
		{
			name: "windows via powershell fallback",
			cfg:  config.HostConfig{OS: "windows"},
			mock: &testutil.MockExecutor{
				RunFn: func(cmd string) (string, error) {
					if strings.Contains(cmd, "uname") {
						return "", assertCalledError()
					}
					if strings.Contains(cmd, "powershell") {
						return "WIN32", nil
					}
					return "", nil
				},
			},
			want: "windows",
		},
		{
			name: "windows via pwsh fallback",
			cfg:  config.HostConfig{OS: "windows"},
			mock: &testutil.MockExecutor{
				RunFn: func(cmd string) (string, error) {
					if strings.Contains(cmd, "uname") {
						return "", assertCalledError()
					}
					if strings.Contains(cmd, "pwsh") {
						return "WIN32", nil
					}
					return "", nil
				},
			},
			want: "windows",
		},
		{
			name: "unknown uname output",
			cfg:  config.HostConfig{OS: "linux"},
			mock: &testutil.MockExecutor{Output: "FreeBSD"},
			want: "",
			err:  true,
		},
		{
			name: "all probes fail",
			cfg:  config.HostConfig{OS: "linux"},
			mock: &testutil.MockExecutor{
				RunFn: func(cmd string) (string, error) {
					return "", assertCalledError()
				},
			},
			want: "",
			err:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newMockHost("test", tc.cfg, tc.mock)
			got, err := DetectOS(h)
			if tc.err {
				if err == nil {
					t.Errorf("expected error, got os=%q", got)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if got != tc.want {
				t.Errorf("DetectOS = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDetectArch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  config.HostConfig
		mock *testutil.MockExecutor
		want string
		err  bool
	}{
		{
			name: "amd64 via uname",
			cfg:  config.HostConfig{OS: "linux"},
			mock: &testutil.MockExecutor{Output: "x86_64"},
			want: "amd64",
		},
		{
			name: "arm64 via uname",
			cfg:  config.HostConfig{OS: "linux"},
			mock: &testutil.MockExecutor{Output: "aarch64"},
			want: "arm64",
		},
		{
			name: "windows via powershell",
			cfg:  config.HostConfig{OS: "windows"},
			mock: &testutil.MockExecutor{
				RunFn: func(cmd string) (string, error) {
					if strings.Contains(cmd, "uname") {
						return "", assertCalledError()
					}
					if strings.Contains(cmd, "powershell") {
						return "AMD64", nil
					}
					return "", nil
				},
			},
			want: "amd64",
		},
		{
			name: "windows via pwsh",
			cfg:  config.HostConfig{OS: "windows"},
			mock: &testutil.MockExecutor{
				RunFn: func(cmd string) (string, error) {
					if strings.Contains(cmd, "uname") {
						return "", assertCalledError()
					}
					if strings.Contains(cmd, "powershell") {
						return "", assertCalledError()
					}
					if strings.Contains(cmd, "pwsh") {
						return "ARM64", nil
					}
					return "", nil
				},
			},
			want: "arm64",
		},
		{
			name: "unsupported arch",
			cfg:  config.HostConfig{OS: "linux"},
			mock: &testutil.MockExecutor{Output: "i386"},
			want: "",
			err:  true,
		},
		{
			name: "all probes fail",
			cfg:  config.HostConfig{OS: "linux"},
			mock: &testutil.MockExecutor{
				RunFn: func(cmd string) (string, error) {
					return "", assertCalledError()
				},
			},
			want: "",
			err:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newMockHost("test", tc.cfg, tc.mock)
			got, err := DetectArch(h)
			if tc.err {
				if err == nil {
					t.Errorf("expected error, got arch=%q", got)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if got != tc.want {
				t.Errorf("DetectArch = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDetectDockerAvailable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  config.HostConfig
		mock *testutil.MockExecutor
		want bool
	}{
		{
			name: "linux docker available",
			cfg:  config.HostConfig{OS: "linux"},
			mock: &testutil.MockExecutor{Output: "24.0.0"},
			want: true,
		},
		{
			name: "linux docker unavailable",
			cfg:  config.HostConfig{OS: "linux"},
			mock: &testutil.MockExecutor{
				RunErr: assertCalledError(),
			},
			want: false,
		},
		{
			name: "linux docker returns empty",
			cfg:  config.HostConfig{OS: "linux"},
			mock: &testutil.MockExecutor{Output: ""},
			want: false,
		},
		{
			name: "windows docker available",
			cfg:  config.HostConfig{OS: "windows"},
			mock: &testutil.MockExecutor{Output: "24.0.0"},
			want: true,
		},
		{
			name: "darwin docker available",
			cfg:  config.HostConfig{OS: "darwin"},
			mock: &testutil.MockExecutor{Output: "24.0.0"},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newMockHost("test", tc.cfg, tc.mock)
			got := DetectDockerAvailable(h)
			if got != tc.want {
				t.Errorf("DetectDockerAvailable = %v, want %v", got, tc.want)
			}
		})
	}
}

func assertCalledError() error {
	return calledError{}
}

type calledError struct{}

func (calledError) Error() string { return "called" }

func TestProbeWindowsShell(t *testing.T) {
	t.Parallel()

	yes := func(string) bool { return true }
	containsWin := func(s string) bool { return strings.Contains(strings.ToLower(s), "win") }

	cases := []struct {
		name   string
		mock   *testutil.MockExecutor
		cmd    string
		match  func(string) bool
		want   string
		wantOK bool
	}{
		{
			name: "primary probe succeeds and matches",
			mock: &testutil.MockExecutor{
				RunFn: func(cmd string) (string, error) {
					if strings.Contains(cmd, "powershell") {
						return "WIN32\n", nil
					}
					return "", assertCalledError() // pwsh must not be called
				},
			},
			cmd:    "[Environment]::OSVersion.Platform",
			match:  containsWin,
			want:   "WIN32",
			wantOK: true,
		},
		{
			name: "primary probe succeeds but does not match; pwsh fallback matches",
			mock: &testutil.MockExecutor{
				RunFn: func(cmd string) (string, error) {
					if strings.Contains(cmd, "powershell") {
						return "Unix\n", nil // present but doesn't match
					}
					if strings.Contains(cmd, "pwsh") {
						return "Win32NT\n", nil
					}
					return "", assertCalledError()
				},
			},
			cmd:    "[Environment]::OSVersion.Platform",
			match:  containsWin,
			want:   "Win32NT",
			wantOK: true,
		},
		{
			name: "primary probe errors; pwsh fallback matches",
			mock: &testutil.MockExecutor{
				RunFn: func(cmd string) (string, error) {
					if strings.Contains(cmd, "powershell") {
						return "", assertCalledError()
					}
					if strings.Contains(cmd, "pwsh") {
						return "ARM64\n", nil
					}
					return "", assertCalledError()
				},
			},
			cmd:    "$env:PROCESSOR_ARCHITECTURE",
			match:  yes,
			want:   "ARM64",
			wantOK: true,
		},
		{
			name: "primary probe errors; pwsh fallback also errors",
			mock: &testutil.MockExecutor{
				RunFn: func(string) (string, error) {
					return "", assertCalledError()
				},
			},
			cmd:    "[Environment]::OSVersion.Platform",
			match:  containsWin,
			want:   "",
			wantOK: false,
		},
		{
			name: "primary and pwsh both succeed but neither matches",
			mock: &testutil.MockExecutor{
				RunFn: func(string) (string, error) {
					return "Linux\n", nil
				},
			},
			cmd:    "[Environment]::OSVersion.Platform",
			match:  containsWin,
			want:   "",
			wantOK: false,
		},
		{
			name: "trims whitespace before matching",
			mock: &testutil.MockExecutor{
				RunFn: func(string) (string, error) {
					return "  WIN32  \n", nil
				},
			},
			cmd:    "[Environment]::OSVersion.Platform",
			match:  containsWin,
			want:   "WIN32",
			wantOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newMockHost("test", config.HostConfig{OS: "windows"}, tc.mock)
			got, ok := probeWindowsShell(h, tc.cmd, tc.match)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("output = %q, want %q", got, tc.want)
			}
		})
	}
}

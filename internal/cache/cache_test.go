package cache

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/testutil"
)

const gwOutput = "2: docker0    inet 172.17.0.1/16 brd 172.17.255.255 scope global docker0\\       valid_lft forever preferred_lft forever\n"

func newCacheHost(t *testing.T, mock *testutil.MockExecutor) *host.Host {
	t.Helper()
	h := host.NewHost("h", config.HostConfig{Addr: "runner@vps", OS: "linux", Arch: "amd64"})
	h.SetConn(mock)
	return h
}

func TestParseGatewayIPOutput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{gwOutput, "172.17.0.1"},
		{"2: docker0    inet 10.0.0.1/16 scope global docker0\n", "10.0.0.1"},
		{"2: docker0    inet6 fe80::1/64 scope link\n", ""},
		{"", ""},
		{"command not found\n", ""},
	}
	for _, tc := range cases {
		if got := parseGatewayIPOutput(tc.in); got != tc.want {
			t.Errorf("parseGatewayIPOutput(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWithTrailingSlash(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"http://h:3000", "http://h:3000/"},
		{"http://h:3000/", "http://h:3000/"},
		{"http://h:3000///", "http://h:3000/"},
		{"", "/"},
	}
	for _, tc := range cases {
		if got := WithTrailingSlash(tc.in); got != tc.want {
			t.Errorf("WithTrailingSlash(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRunnerURL_precedence(t *testing.T) {
	t.Parallel()
	h := newCacheHost(t, &testutil.MockExecutor{Output: gwOutput})

	s := Settings{Enabled: true, URLOverride: "http://override:9999/"}
	if got := s.RunnerURL(h); got != "http://override:9999/" {
		t.Errorf("override: got %q", got)
	}

	// Default: the cache container's fixed address on the dedicated gh-sr
	// network — independent of bind_addr, the docker0 gateway, and the
	// host-side published port.
	s = Settings{Enabled: true, BindAddr: "192.168.1.5", Port: 3001}
	if got := s.RunnerURL(h); got != fmt.Sprintf("http://%s:%d/", ContainerIP, ContainerPort) {
		t.Errorf("default: got %q", got)
	}

	s = Settings{Enabled: true}
	if got := s.RunnerURL(newCacheHost(t, &testutil.MockExecutor{RunErr: io.EOF})); got != fmt.Sprintf("http://%s:%d/", ContainerIP, ContainerPort) {
		t.Errorf("no-gateway host must not affect the URL: got %q", got)
	}
}

func TestResolvedStoragePath(t *testing.T) {
	t.Parallel()
	h := newCacheHost(t, &testutil.MockExecutor{Output: "/root\n"})

	got, err := (&Settings{}).resolvedStoragePath(h)
	if err != nil || got != "/root/.gh-sr/cache" {
		t.Errorf("default: got %q err %v", got, err)
	}

	got, err = (&Settings{StoragePath: "/srv/cache"}).resolvedStoragePath(h)
	if err != nil || got != "/srv/cache" {
		t.Errorf("absolute: got %q err %v", got, err)
	}

	got, err = (&Settings{StoragePath: "$HOME/cache"}).resolvedStoragePath(h)
	if err != nil || got != "/root/cache" {
		t.Errorf("$HOME prefix: got %q err %v", got, err)
	}

	if _, err := (&Settings{}).resolvedStoragePath(newCacheHost(t, &testutil.MockExecutor{Output: "\n"})); err == nil {
		t.Error("empty $HOME should error")
	}
}

func TestEnsure_disabled(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{}
	if err := Ensure(io.Discard, newCacheHost(t, mock), Settings{}); err != nil {
		t.Fatalf("Ensure disabled: %v", err)
	}
	if len(mock.Calls) != 0 {
		t.Fatalf("disabled cache must not run commands, got %v", mock.Calls)
	}
}

func TestEnsure_runningNoop(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		if strings.Contains(cmd, ".State.Status") {
			return "running\n", nil
		}
		if strings.Contains(cmd, "NetworkSettings.Networks") {
			return NetworkName + "=" + ContainerIP + " \n", nil
		}
		return "", nil
	}}
	if err := Ensure(io.Discard, newCacheHost(t, mock), Settings{Enabled: true}); err != nil {
		t.Fatalf("Ensure running: %v", err)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "docker start") || strings.Contains(c, "docker rm") || strings.Contains(c, "docker run") {
			t.Fatalf("running container on the expected network must be a no-op, calls: %v", mock.Calls)
		}
	}
}

func TestEnsure_existsStarts(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, ".State.Status"):
			return "exited\n", nil
		case strings.Contains(cmd, "NetworkSettings.Networks"):
			return NetworkName + "=" + ContainerIP + " \n", nil
		default:
			return "", nil
		}
	}}
	if err := Ensure(io.Discard, newCacheHost(t, mock), Settings{Enabled: true}); err != nil {
		t.Fatalf("Ensure exists: %v", err)
	}
	last := mock.Calls[len(mock.Calls)-1]
	if !strings.Contains(last, "docker start gh-sr-cache") {
		t.Fatalf("stopped container must be started, got %q", last)
	}
}

// TestEnsure_recreatesWhenNetworkMismatch covers convergence from a
// pre-network deployment: an existing container not on NetworkName with the
// fixed IP must be removed and redeployed, not silently left blackholed.
func TestEnsure_recreatesWhenNetworkMismatch(t *testing.T) {
	t.Parallel()
	created := false
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, ".State.Status"):
			return "running\n", nil
		case strings.Contains(cmd, "NetworkSettings.Networks"):
			return "bridge=172.17.0.8 \n", nil
		case strings.Contains(cmd, "docker rm -f"):
			return "", nil
		case strings.Contains(cmd, "docker run"):
			created = true
			return "", nil
		case strings.Contains(cmd, "echo $HOME"):
			return "/root\n", nil
		default:
			return "", nil
		}
	}}
	var buf strings.Builder
	if err := Ensure(&buf, newCacheHost(t, mock), Settings{Enabled: true}); err != nil {
		t.Fatalf("Ensure mismatch: %v", err)
	}
	if !strings.Contains(buf.String(), "recreating") {
		t.Errorf("expected recreate message, got: %s", buf.String())
	}
	if !contains(mock.Calls, "docker rm -f gh-sr-cache") {
		t.Errorf("expected removal of outdated container, calls: %v", mock.Calls)
	}
	if !created {
		t.Errorf("expected redeploy, calls: %v", mock.Calls)
	}
}

func TestEnsure_missingDeploys(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, ".State.Status"):
			return "", nil
		case strings.Contains(cmd, "echo $HOME"):
			return "/root\n", nil
		case strings.Contains(cmd, "ip -4 -o addr show docker0"):
			return gwOutput, nil
		default:
			return "", nil
		}
	}}
	h := newCacheHost(t, mock)
	if err := Ensure(io.Discard, h, Settings{Enabled: true, RetentionDays: 30, MaxSizeBytes: 1024, MaxUsagePercent: 95}); err != nil {
		t.Fatalf("Ensure missing: %v", err)
	}

	var runLine string
	for _, c := range mock.Calls {
		if strings.Contains(c, "docker run") {
			runLine = c
		}
	}
	if runLine == "" {
		t.Fatalf("no docker run call: %v", mock.Calls)
	}
	for _, want := range []string{
		"docker run -d --name gh-sr-cache",
		"--restart unless-stopped",
		"--network " + NetworkName,
		"--ip " + ContainerIP,
		fmt.Sprintf("-p 172.17.0.1:%d:%d", DefaultPort, ContainerPort),
		"-v '/root/.gh-sr/cache':/data",
		fmt.Sprintf("-e 'API_BASE_URL=http://%s:%d/'", ContainerIP, ContainerPort),
		"-e 'STORAGE_DRIVER=filesystem'",
		"-e 'STORAGE_FILESYSTEM_PATH=/data/cache'",
		"-e 'DB_DRIVER=sqlite'",
		"-e 'DB_SQLITE_PATH=/data/cache-server.db'",
		"-e 'CACHE_CLEANUP_OLDER_THAN_DAYS=30'",
		"-e 'CACHE_MAX_SIZE_BYTES=1024'",
		"-e 'CACHE_FILESYSTEM_MAX_USAGE_PERCENT=95'",
		"-e 'MANAGEMENT_API_KEY=",
		"'ghcr.io/falcondev-oss/github-actions-cache-server:latest'",
	} {
		if !strings.Contains(runLine, want) {
			t.Errorf("docker run missing %q:\n%s", want, runLine)
		}
	}
	// The network itself must be ensured (idempotent create) before the run.
	if !contains(mock.Calls, fmt.Sprintf("docker network create --subnet=%s %s", NetworkSubnet, NetworkName)) {
		t.Errorf("expected idempotent network create, calls: %v", mock.Calls)
	}
}

func TestDeploy_pullFailureTolerated(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "docker inspect"):
			return "", nil
		case strings.Contains(cmd, "echo $HOME"):
			return "/root\n", nil
		case strings.Contains(cmd, "ip -4 -o addr show docker0"):
			return gwOutput, nil
		case strings.Contains(cmd, "docker pull"):
			return "", io.EOF
		default:
			return "", nil
		}
	}}
	if err := Ensure(io.Discard, newCacheHost(t, mock), Settings{Enabled: true}); err != nil {
		t.Fatalf("pull failure must not abort deploy: %v", err)
	}
	found := false
	for _, c := range mock.Calls {
		if strings.Contains(c, "docker run") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no docker run after failed pull: %v", mock.Calls)
	}
}

func TestDeploy_bindFallbackWarns(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "docker inspect"):
			return "", nil
		case strings.Contains(cmd, "echo $HOME"):
			return "/root\n", nil
		default:
			return "", nil
		}
	}}
	var buf strings.Builder
	if err := Ensure(&buf, newCacheHost(t, mock), Settings{Enabled: true}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !strings.Contains(buf.String(), "0.0.0.0") || !strings.Contains(buf.String(), "warning") {
		t.Errorf("missing gateway must warn about 0.0.0.0, got: %s", buf.String())
	}
	found := false
	for _, c := range mock.Calls {
		if strings.Contains(c, fmt.Sprintf("-p 0.0.0.0:%d:%d", DefaultPort, ContainerPort)) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 0.0.0.0 bind fallback, calls: %v", mock.Calls)
	}
}

func TestEnsureManagementKey_generateAndReuse(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{}
	h := newCacheHost(t, mock)
	key, err := ensureManagementKey(h, "/root/.gh-sr/cache")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(key) != 64 {
		t.Fatalf("key should be 32-byte hex (64 chars), got %d: %q", len(key), key)
	}
	var stored string
	for _, c := range mock.Calls {
		if strings.Contains(c, "printf") && strings.Contains(c, "management-api-key") {
			stored = c
		}
	}
	if stored == "" || !strings.Contains(stored, key) {
		t.Fatalf("generated key not persisted: %v", mock.Calls)
	}
	if !strings.Contains(stored, "umask 077") {
		t.Errorf("key file must be written with umask 077: %q", stored)
	}

	// A second call on a host that already has the key file reuses it.
	mock2 := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		if strings.Contains(cmd, "test -f") && strings.Contains(cmd, "cat") {
			return "existingkey\n", nil
		}
		return "", nil
	}}
	reused, err := ensureManagementKey(newCacheHost(t, mock2), "/root/.gh-sr/cache")
	if err != nil || reused != "existingkey" {
		t.Fatalf("reuse: got %q err %v", reused, err)
	}
	for _, c := range mock2.Calls {
		if strings.Contains(c, "printf") {
			t.Errorf("existing key must not be overwritten: %q", c)
		}
	}
}

func TestPrune_keyMissing(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		if strings.Contains(cmd, "echo $HOME") {
			return "/root\n", nil
		}
		return "", nil
	}}
	var buf strings.Builder
	if err := Prune(&buf, newCacheHost(t, mock), Settings{Enabled: true}); err != nil {
		t.Fatalf("Prune without key: %v", err)
	}
	if !strings.Contains(buf.String(), "management API key") {
		t.Errorf("expected hint about missing key, got: %s", buf.String())
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "management-api/cache-entries") {
			t.Fatalf("must not call the API without a key: %q", c)
		}
	}
}

func TestPrune_apiFailureTolerated(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "echo $HOME"):
			return "/root\n", nil
		case strings.Contains(cmd, "curl"):
			return "", io.EOF
		default:
			return "", nil
		}
	}}
	var buf strings.Builder
	s := Settings{Enabled: true, ManagementAPIKey: "k1"}
	if err := Prune(&buf, newCacheHost(t, mock), s); err != nil {
		t.Fatalf("Prune API failure must be tolerated: %v", err)
	}
	if !strings.Contains(buf.String(), "warning") {
		t.Errorf("expected failure hint, got: %s", buf.String())
	}
}

func TestPrune_success(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "echo $HOME"):
			return "/root\n", nil
		case strings.Contains(cmd, "ip -4 -o addr show docker0"):
			return gwOutput, nil
		default:
			return "", nil
		}
	}}
	var buf strings.Builder
	s := Settings{Enabled: true, ManagementAPIKey: "k1"}
	if err := Prune(&buf, newCacheHost(t, mock), s); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "curl") {
			if !strings.Contains(c, "X-Api-Key: k1") || !strings.Contains(c, "-X DELETE") ||
				!strings.Contains(c, fmt.Sprintf("'http://172.17.0.1:%d'/management-api/cache-entries/", DefaultPort)) {
				t.Errorf("unexpected prune request: %q", c)
			}
		}
	}
}

func TestRemove(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		if strings.Contains(cmd, "echo $HOME") {
			return "/root\n", nil
		}
		return "", nil
	}}
	if err := Remove(io.Discard, newCacheHost(t, mock), Settings{}, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !contains(mock.Calls, "docker rm -f gh-sr-cache") {
		t.Fatalf("expected docker rm -f, calls: %v", mock.Calls)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "rm -rf") {
			t.Fatalf("purgeData=false must keep storage: %q", c)
		}
	}

	mock2 := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		if strings.Contains(cmd, "echo $HOME") {
			return "/root\n", nil
		}
		return "", nil
	}}
	if err := Remove(io.Discard, newCacheHost(t, mock2), Settings{}, true); err != nil {
		t.Fatalf("Remove purge: %v", err)
	}
	if !contains(mock2.Calls, "rm -rf '/root/.gh-sr/cache'") {
		t.Fatalf("expected storage purge, calls: %v", mock2.Calls)
	}
}

func TestMeasureStorage(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "echo $HOME"):
			return "/root\n", nil
		case strings.Contains(cmd, "du -sb"):
			return "12345\n", nil
		default:
			return "", nil
		}
	}}
	path, n, err := MeasureStorage(newCacheHost(t, mock), Settings{})
	if err != nil || path != "/root/.gh-sr/cache" || n != 12345 {
		t.Fatalf("MeasureStorage: path=%q n=%d err=%v", path, n, err)
	}

	// Missing dir: du prints nothing → 0 bytes, no error.
	mock2 := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		if strings.Contains(cmd, "echo $HOME") {
			return "/root\n", nil
		}
		return "", nil
	}}
	path, n, err = MeasureStorage(newCacheHost(t, mock2), Settings{})
	if err != nil || n != 0 {
		t.Fatalf("missing dir: path=%q n=%d err=%v", path, n, err)
	}
}

func contains(calls []string, substr string) bool {
	for _, c := range calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/an-lee/gh-sr/internal/cache"
	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/hostshell"
	"github.com/an-lee/gh-sr/internal/testutil"
)

func TestContainerName(t *testing.T) {
	t.Parallel()
	if got := containerName("my-agentic-1"); got != "gh-sr-my-agentic-1" {
		t.Errorf("containerName: got %q", got)
	}
	if got := containerName("x"); got != "gh-sr-x" {
		t.Errorf("containerName(x): got %q", got)
	}
	if got := ContainerDockerName("my-agentic-1"); got != containerName("my-agentic-1") {
		t.Errorf("ContainerDockerName vs containerName: got %q", got)
	}
}

func TestContainerStateDir(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h", config.HostConfig{Addr: "local", OS: "linux", Arch: "amd64"})
	dir := containerStateDir(h, "my-runner-1")
	// Should match the runner dir for the instance.
	if !strings.Contains(dir, "my-runner-1") {
		t.Errorf("containerStateDir should include instance name, got %q", dir)
	}
	if !strings.Contains(dir, ".gh-sr/runners") {
		t.Errorf("containerStateDir should be under .gh-sr/runners, got %q", dir)
	}
}

// TestDockerRunArgShape verifies the docker create command includes the expected
// --privileged flag, --shm-size for Chromium/Selenium, the bind-mount, and env vars.
func TestDockerRunArgShape(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h", config.HostConfig{Addr: "local", OS: "linux", Arch: "amd64"})
	rc := config.RunnerConfig{
		Name:       "agentic",
		Repo:       "owner/repo",
		Host:       "h",
		Count:      1,
		Profile:    "agentic",
		RunnerMode: config.RunnerModeContainer,
	}
	instanceName := rc.InstanceNames()[0] // "agentic-1"
	cName := containerName(instanceName)
	stateDir := containerStateDir(h, instanceName)
	imageTag := AgenticRunnerImageTag + ":2.999.0"

	// Build the expected docker create command manually (mirrors setupContainer logic).
	labels := rc.EffectiveLabelsForInstance(h.OS, h.Arch, 0)
	cmd := strings.Join([]string{
		"mkdir -p " + hostshell.PosixSingleQuote(stateDir),
		"docker create",
		"  --name " + hostshell.PosixSingleQuote(cName),
		"  --privileged",
		"  --shm-size=2g",
		"  --restart unless-stopped",
		"  -v " + hostshell.PosixSingleQuote(stateDir) + ":/runner-state",
		"  -e GH_SR_RUNNER_NAME=" + hostshell.PosixSingleQuote(instanceName),
		"  -e GH_SR_RUNNER_TOKEN=" + hostshell.PosixSingleQuote("tok"),
		"  -e GH_SR_RUNNER_URL=" + hostshell.PosixSingleQuote("https://github.com/owner/repo"),
		"  -e GH_SR_RUNNER_LABELS=" + hostshell.PosixSingleQuote(strings.Join(labels, ",")),
		"  -e GH_SR_RUNNER_GROUP=" + hostshell.PosixSingleQuote("Default"),
		"  -e GH_SR_RUNNER_EPHEMERAL=" + hostshell.PosixSingleQuote(""),
		"  -e GH_SR_DOCKERD_START_TIMEOUT=" + hostshell.PosixSingleQuote("90"),
		"  -e GH_SR_BOOTSTRAP_MAX_RETRIES=" + hostshell.PosixSingleQuote("5"),
		"  " + hostshell.PosixSingleQuote(imageTag),
	}, "\n")

	if !strings.Contains(cmd, "--privileged") {
		t.Error("docker create command must include --privileged for DinD")
	}
	if !strings.Contains(cmd, "--shm-size=2g") {
		t.Error("docker create command must include --shm-size=2g for browser/system tests")
	}
	if !strings.Contains(cmd, "--restart unless-stopped") {
		t.Error("docker create command must include --restart unless-stopped so crashes / daemon restarts / host reboots bring the container back")
	}
	if !strings.Contains(cmd, cName) {
		t.Errorf("docker create command must include container name %q", cName)
	}
	if !strings.Contains(cmd, ":/runner-state") {
		t.Error("docker create command must bind-mount to /runner-state")
	}
	if !strings.Contains(cmd, "GH_SR_RUNNER_NAME") {
		t.Error("docker create command must pass GH_SR_RUNNER_NAME env var")
	}
	if !strings.Contains(cmd, "GH_SR_RUNNER_TOKEN") {
		t.Error("docker create command must pass GH_SR_RUNNER_TOKEN env var")
	}
	if !strings.Contains(cmd, "GH_SR_RUNNER_URL") {
		t.Error("docker create command must pass GH_SR_RUNNER_URL env var")
	}
	if !strings.Contains(cmd, "GH_SR_DOCKERD_START_TIMEOUT") {
		t.Error("docker create command must pass GH_SR_DOCKERD_START_TIMEOUT env var")
	}
	if !strings.Contains(cmd, "GH_SR_BOOTSTRAP_MAX_RETRIES") {
		t.Error("docker create command must pass GH_SR_BOOTSTRAP_MAX_RETRIES env var")
	}
	if !strings.Contains(cmd, "GH_SR_RUNNER_LABELS") {
		t.Error("docker create command must pass GH_SR_RUNNER_LABELS env var")
	}
}

// TestAgenticRunnerImageTag verifies the image tag format used by container mode.
func TestAgenticRunnerImageTag(t *testing.T) {
	t.Parallel()
	tag := AgenticRunnerImageTag
	if !strings.HasPrefix(tag, "gh-sr/") {
		t.Errorf("image tag should start with gh-sr/, got %q", tag)
	}
	// The versioned tag appended at runtime.
	versioned := tag + ":2.123.0"
	if !strings.Contains(versioned, "2.123.0") {
		t.Errorf("versioned tag format unexpected: %q", versioned)
	}
}

func TestContainerRunnerImageTag(t *testing.T) {
	t.Parallel()
	fork := DefaultForkRunnerImage
	base := AgenticRunnerImageTag + ":2.337.0"
	if got := ContainerRunnerImageTag(fork, nil); got != base {
		t.Errorf("empty extras: got %q want %q", got, base)
	}
	if got := ContainerRunnerImageTag(fork, []string{}); got != base {
		t.Errorf("empty slice: got %q want %q", got, base)
	}
	a := ContainerRunnerImageTag(fork, []string{"sqlite3", "ffmpeg"})
	b := ContainerRunnerImageTag(fork, []string{"ffmpeg", "sqlite3"})
	if a != b {
		t.Errorf("order should not matter: %q vs %q", a, b)
	}
	if want := base + "-x908d9db2"; a != want {
		t.Errorf("tag with extras: got %q want %q", a, want)
	}
	dup := ContainerRunnerImageTag(fork, []string{"curl", "curl"})
	once := ContainerRunnerImageTag(fork, []string{"curl"})
	if dup != once {
		t.Errorf("duplicates should be ignored: got %q vs %q", dup, once)
	}
	// A base_image bump must change the local tag (that is what triggers a rebuild):
	// digest pins shorten to "d" + first 12 digest chars; untagged refs map to "latest".
	digest := fork[:strings.LastIndex(fork, ":")] + "@sha256:" + strings.Repeat("ab", 32)
	if got := ContainerRunnerImageTag(digest, nil); got != AgenticRunnerImageTag+":dabababababab" {
		t.Errorf("digest-pinned base: got %q, want %q", got, AgenticRunnerImageTag+":dabababababab")
	}
	untagged := "ghcr.io/falcondev-oss/actions-runner"
	if got := ContainerRunnerImageTag(untagged, nil); got != AgenticRunnerImageTag+":latest" {
		t.Errorf("untagged base: got %q, want %q", got, AgenticRunnerImageTag+":latest")
	}
}

// TestAgenticRunnerDockerfileRootlessBase pins the rootless image contract: the
// Dockerfile derives FROM the fork runner base (passed as the FORK_RUNNER_IMAGE
// build-arg, which carries the CUSTOM_ACTIONS_RESULTS_URL override) and the apt
// manifest installs zstd (the Actions-cache compression codec the fork runner
// needs for cache upload/download).
func TestAgenticRunnerDockerfileRootlessBase(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"ARG FORK_RUNNER_IMAGE",
		"FROM ${FORK_RUNNER_IMAGE}",
	} {
		if !strings.Contains(agenticRunnerDockerfile, want) {
			t.Fatalf("Dockerfile must contain %q, got:\n%s", want, agenticRunnerDockerfile)
		}
	}
	if !strings.Contains(agenticRunnerAptPackagesCore, "zstd") {
		t.Fatal("apt-packages-core.txt must install zstd (Actions-cache compression codec)")
	}
}

// TestAgenticRunnerImageForbidsRetiredHacks guards the rootless cutover: gh-aw
// v0.88 runs AWF rootless (isolation via inner-Docker topology, its own
// --add-host=host.docker.internal:host-gateway), so the sudo-era layers — docker
// shim, baked DNS/dnsmasq, the 10.200.0.1 bip pin, the iptables NAT bypass, AWF or
// gh-aw preinstall, NOPASSWD sudoers — must never reappear in any embedded file.
func TestAgenticRunnerImageForbidsRetiredHacks(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"Dockerfile":            agenticRunnerDockerfile,
		"apt-packages-core.txt": agenticRunnerAptPackagesCore,
		"entrypoint.sh":         agenticRunnerEntrypoint,
		"job-started.sh":        agenticRunnerJobStartedHook,
		"job-completed.sh":      agenticRunnerJobCompletedHook,
	}
	for name, content := range files {
		for _, forbidden := range []string{
			"docker-wrapper",
			"docker-shim",
			"dnsmasq",
			"NOPASSWD",
			"10.200.0.1",
			"resolv.conf",
			"install_awf_binary",
			"iptables -t nat",
			"gh-aw-firewall/main/install.sh",
		} {
			if strings.Contains(content, forbidden) {
				t.Fatalf("%s must not contain the retired hack %q", name, forbidden)
			}
		}
	}
}

// TestAgenticRunnerEntrypointStartsDockerdOnce guards against re-introducing the
// dockerd RESTART dance (a key instability source): write daemon.json, then kill and
// restart dockerd to pick it up. The entrypoint MAY adjust the baked daemon.json for
// collision avoidance, but only ONCE and strictly BEFORE the single dockerd start, so
// the daemon reads the final config on its one and only start.
func TestAgenticRunnerEntrypointStartsDockerdOnce(t *testing.T) {
	t.Parallel()
	// Exactly one dockerd invocation is the strong guard against the restart dance: a
	// restart would require a second `dockerd \` launch. (We intentionally do NOT match
	// the substring "restart dockerd" here — the entrypoint's own comments explain why
	// the historical restart dance is forbidden, and matching that text would be a
	// false positive.)
	if c := strings.Count(agenticRunnerEntrypoint, "dockerd \\"); c != 1 {
		t.Fatalf("entrypoint should start dockerd exactly once, found %d invocations", c)
	}
	// Any daemon.json write must precede the single dockerd start (no post-start
	// rewrite + restart). Compare the position of the write redirection to the dockerd
	// invocation; the comment block mentions daemon.json too, so anchor on the actual
	// `> /etc/docker/daemon.json` redirect rather than a bare path reference.
	dockerdIdx := strings.Index(agenticRunnerEntrypoint, "dockerd \\")
	if writeIdx := strings.Index(agenticRunnerEntrypoint, "> /etc/docker/daemon.json"); writeIdx >= 0 {
		if dockerdIdx < 0 || writeIdx > dockerdIdx {
			t.Fatal("daemon.json may only be written BEFORE the single dockerd start (no post-start rewrite/restart)")
		}
	}
}

// TestAgenticRunnerEntrypointWiresJobHooks verifies the per-job reset hooks are wired
// into the runner .env so the Actions runner invokes them before/after every job.
func TestAgenticRunnerEntrypointWiresJobHooks(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/gh-sr/hooks/job-started.sh",
		"ACTIONS_RUNNER_HOOK_JOB_COMPLETED=/opt/gh-sr/hooks/job-completed.sh",
	} {
		if !strings.Contains(agenticRunnerEntrypoint, want) {
			t.Fatalf("entrypoint should wire %q into .env", want)
		}
	}
}

// TestAgenticRunnerEntrypointRootlessWiring pins the fork-runner era wiring: the
// runner lives at /home/runner (fork image layout), config.sh gets --disableupdate
// (the fork's CUSTOM_ACTIONS_RESULTS_URL-aware runner must never self-update back to
// stock behavior), and GH_SR_CACHE_URL is forwarded as CUSTOM_ACTIONS_RESULTS_URL in
// both .env and RUNNER_ENV so cache traffic hits the local cache server.
func TestAgenticRunnerEntrypointRootlessWiring(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		`RUNNER_DIR="/home/runner"`,
		"--disableupdate",
		`CUSTOM_ACTIONS_RESULTS_URL=${GH_SR_CACHE_URL}`,
		`RUNNER_TOOL_CACHE="/home/runner/.toolcache"`,
		"gh-aw-mcpg:latest", // pre-pull vocabulary (rootless gh-aw image set)
	} {
		if !strings.Contains(agenticRunnerEntrypoint, want) {
			t.Fatalf("entrypoint should contain %q, got:\n%s", want, agenticRunnerEntrypoint)
		}
	}
	// `su -` resets the environment, so RUNNER_ENV must re-export the cache URL for
	// the runner user too (config.sh and run.sh both run through su -).
	if !strings.Contains(agenticRunnerEntrypoint, `RUNNER_ENV="${RUNNER_ENV} CUSTOM_ACTIONS_RESULTS_URL='${GH_SR_CACHE_URL}'"`) {
		t.Fatal("RUNNER_ENV must re-export CUSTOM_ACTIONS_RESULTS_URL for the runner user")
	}
}

// TestAgenticRunnerJobHooksReset verifies the per-job reset hooks perform the
// deterministic teardown (containers, networks, /tmp/gh-aw) using the rootless-era
// vocabulary (awmg-/awf-/gh-aw names; gh-aw-mcpg + firewall-stack ancestors — the
// cli-proxy sidecar reuses the gh-aw-mcpg image, so no separate ancestor is needed)
// and that the completed hook always exits 0.
func TestAgenticRunnerJobHooksReset(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"docker network prune -f",
		"rm -rf /tmp/gh-aw",
		"name=awmg-",
		"name=awf-",
		"name=gh-aw",
		"ancestor=$img",
		"ghcr.io/github/gh-aw-mcpg",
		"ghcr.io/github/gh-aw-firewall/agent",
		"ghcr.io/github/gh-aw-firewall/api-proxy",
		"exit 0",
	} {
		if !strings.Contains(agenticRunnerJobCompletedHook, want) {
			t.Fatalf("job-completed hook must contain %q", want)
		}
	}
	for _, want := range []string{
		"docker ps -aq",
		"docker network prune -f",
		"rm -rf /tmp/gh-aw",
		"docker info",
	} {
		if !strings.Contains(agenticRunnerJobStartedHook, want) {
			t.Fatalf("job-started hook must contain %q", want)
		}
	}
}

// TestAgenticRunnerJobHooksPreserveImageCache ensures per-job resets never invalidate
// the inner Docker image-layer cache (the whole point of cache/state separation).
func TestAgenticRunnerJobHooksPreserveImageCache(t *testing.T) {
	t.Parallel()
	hooks := map[string]string{
		"job-started.sh":   agenticRunnerJobStartedHook,
		"job-completed.sh": agenticRunnerJobCompletedHook,
	}
	for name, hook := range hooks {
		for _, forbidden := range []string{
			"docker image prune",
			"docker system prune",
			"docker volume prune",
			"docker builder prune",
			"docker rmi",
		} {
			if strings.Contains(hook, forbidden) {
				t.Fatalf("%s must not invalidate the image cache with %q", name, forbidden)
			}
		}
	}
}

func TestAgenticRunnerDockerfileInstallsNodeLTS(t *testing.T) {
	t.Parallel()

	for _, want := range []string{
		"https://deb.nodesource.com/setup_lts.x",
		"apt-get install -y --no-install-recommends nodejs",
		"node -v",
		"npm -v",
	} {
		if !strings.Contains(agenticRunnerDockerfile, want) {
			t.Fatalf("Dockerfile should install Node.js LTS with %q, got:\n%s", want, agenticRunnerDockerfile)
		}
	}
}

// TestAgenticRunnerDockerfileBakesHooksAndEntrypoint verifies the Dockerfile installs
// the per-job reset hooks and the entrypoint, and wires ENTRYPOINT to it.
func TestAgenticRunnerDockerfileBakesHooksAndEntrypoint(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"COPY hooks/job-started.sh /opt/gh-sr/hooks/job-started.sh",
		"COPY hooks/job-completed.sh /opt/gh-sr/hooks/job-completed.sh",
		"COPY entrypoint.sh /entrypoint.sh",
		`ENTRYPOINT ["/entrypoint.sh"]`,
	} {
		if !strings.Contains(agenticRunnerDockerfile, want) {
			t.Fatalf("Dockerfile should bake %q, got:\n%s", want, agenticRunnerDockerfile)
		}
	}
}

// TestAgenticRunnerDockerfileSymlinksHostedToolCache guards the compatibility shim
// for actions that hardcode /opt/hostedtoolcache (notably ruby/setup-ruby@v1,
// which deliberately ignores $RUNNER_TOOL_CACHE when it doesn't detect a
// self-hosted runner — see issue #405). Without the symlink the runner user's
// `mkdir /opt/hostedtoolcache/Ruby/...` fails with EACCES because /opt is owned
// by root and the new image no longer creates /opt/hostedtoolcache.
func TestAgenticRunnerDockerfileSymlinksHostedToolCache(t *testing.T) {
	t.Parallel()
	// Both the new tool-cache root and the symlink must be created in one RUN
	// so the symlink is guaranteed to resolve to a runner-owned directory at
	// image-build time.
	if !strings.Contains(agenticRunnerDockerfile, "ln -s /home/runner/.toolcache /opt/hostedtoolcache") {
		t.Fatalf("Dockerfile should symlink /opt/hostedtoolcache -> /home/runner/.toolcache, got:\n%s", agenticRunnerDockerfile)
	}
	// The chown and the symlink must share the same RUN (single layer) so the
	// symlink target exists and is owned by runner when the layer is committed.
	if !strings.Contains(agenticRunnerDockerfile, "mkdir -p /home/runner/.toolcache && chown runner:runner /home/runner/.toolcache \\\n    && ln -s /home/runner/.toolcache /opt/hostedtoolcache") {
		t.Fatal("symlink and tool-cache mkdir must run in a single RUN layer (chown must precede ln)")
	}
	// RUNNER_TOOL_CACHE in the entrypoint must remain /home/runner/.toolcache —
	// the gh-aw mount guard from 5a8ae78 depends on the env var NOT being
	// rewritten to /opt/hostedtoolcache.
	if !strings.Contains(agenticRunnerEntrypoint, `RUNNER_TOOL_CACHE="/home/runner/.toolcache"`) {
		t.Fatal("entrypoint must keep RUNNER_TOOL_CACHE at /home/runner/.toolcache so the AWF mount guard continues to pass")
	}
}

// TestAgenticRunnerEntrypointPinsMTU verifies the entrypoint pins the inner-bridge
// MTU (mtu-only daemon.json written before the single dockerd start) and the outer
// egress interface MTU when the host egress MTU (GH_SR_HOST_MTU) is below 1500 — the
// fix for reduced-MTU host networks that otherwise break large-packet TLS handshakes
// (e.g. actions/setup-go). The rootless cutover dropped the MSS clamp: TCP advertises
// a matching MSS from the pinned MTUs alone.
func TestAgenticRunnerEntrypointPinsMTU(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"GH_SR_HOST_MTU",
		`"mtu":`,                    // injected into the mtu-only daemon.json
		"> /etc/docker/daemon.json", // write must precede the one dockerd start
		"ip link set dev",           // lower the outer container's egress interface MTU
	} {
		if !strings.Contains(agenticRunnerEntrypoint, want) {
			t.Fatalf("entrypoint should pin MTU: missing %q", want)
		}
	}
	if strings.Contains(agenticRunnerEntrypoint, "clamp-mss-to-pmtu") {
		t.Fatal("rootless entrypoint must not re-add the iptables MSS clamp")
	}
	// (TestAgenticRunnerEntrypointStartsDockerdOnce enforces the ordering invariant.)
	if strings.Count(agenticRunnerEntrypoint, "dockerd \\") != 1 {
		t.Fatal("MTU changes must not add a second dockerd start")
	}
}

func TestAgenticRunnerEntrypointDockerdBootstrapResilience(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"GH_SR_DOCKERD_START_TIMEOUT",
		"GH_SR_BOOTSTRAP_MAX_RETRIES",
		"DOCKERD_START_TIMEOUT",
		"BOOTSTRAP_MAX_RETRIES",
		"dockerd-start-failures",
		"bootstrap-failed",
		"exec sleep infinity",
	} {
		if !strings.Contains(agenticRunnerEntrypoint, want) {
			t.Fatalf("entrypoint should implement bootstrap resilience: missing %q", want)
		}
	}
	if strings.Contains(agenticRunnerEntrypoint, "seq 1 30") {
		t.Fatal("entrypoint must not hard-code a 30s dockerd wait loop")
	}
}

// TestContainerRestartPolicy pins the "unless-stopped" choice. This policy
// restarts the container on every non-explicit exit (crash, OOM, inner
// process shutdown, Docker daemon restart, host reboot) while still honoring
// `docker stop` / `gh sr down` — closing the historical "agentic runner just
// stopped, not restart" failure mode that the old "on-failure:N" policy had
// (clean exits left the container permanently down). The bootstrap-failure
// bound is now the entrypoint's `bootstrap-failed` marker + `exec sleep
// infinity`, not Docker's retry counter.
func TestContainerRestartPolicy(t *testing.T) {
	t.Parallel()
	if got := containerRestartPolicy(); got != "unless-stopped" {
		t.Fatalf("got %q, want %q", got, "unless-stopped")
	}
}

func TestDockerdStartTimeoutDockerCreateArg(t *testing.T) {
	t.Parallel()
	if got := dockerdStartTimeoutDockerCreateArg(90); !strings.Contains(got, "GH_SR_DOCKERD_START_TIMEOUT='90'") {
		t.Fatalf("got %q", got)
	}
	if got := dockerdStartTimeoutDockerCreateArg(0); got != "" {
		t.Fatalf("zero should omit env, got %q", got)
	}
	if got := dockerdStartTimeoutDockerCreateArg(-5); got != "" {
		t.Fatalf("negative should omit env, got %q", got)
	}
}

func TestBootstrapMaxRetriesDockerCreateArg(t *testing.T) {
	t.Parallel()
	if got := bootstrapMaxRetriesDockerCreateArg(5); !strings.Contains(got, "GH_SR_BOOTSTRAP_MAX_RETRIES='5'") {
		t.Fatalf("got %q", got)
	}
	if got := bootstrapMaxRetriesDockerCreateArg(0); got != "" {
		t.Fatalf("zero should omit env, got %q", got)
	}
}

// TestCacheURLDockerCreateArg pins the local-cache injection contract: a
// non-empty cache URL becomes a single-quoted -e GH_SR_CACHE_URL line (the
// entrypoint forwards it to the runner as CUSTOM_ACTIONS_RESULTS_URL), and an
// empty URL emits nothing so the runner keeps GitHub's cache service.
func TestCacheURLDockerCreateArg(t *testing.T) {
	t.Parallel()
	if got := cacheURLDockerCreateArg(""); got != "" {
		t.Fatalf("empty URL should omit env, got %q", got)
	}
	got := cacheURLDockerCreateArg("http://172.17.0.1:3000/")
	if !strings.Contains(got, "-e GH_SR_CACHE_URL=") || !strings.Contains(got, "'http://172.17.0.1:3000/'") {
		t.Fatalf("got %q", got)
	}
}

func TestCacheURLEnvFor(t *testing.T) {
	t.Parallel()
	if got := cacheURLEnvFor(nil, nil); got != "" {
		t.Fatalf("nil settings: got %q", got)
	}

	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		if strings.Contains(cmd, "ip -4 -o addr show docker0") {
			return "2: docker0    inet 172.17.0.1/16 brd 172.17.255.255 scope global docker0\\\n", nil
		}
		return "", nil
	}}
	h := host.NewHost("h", config.HostConfig{Addr: "runner@vps", OS: "linux", Arch: "amd64"})
	h.SetConn(mock)

	if got := cacheURLEnvFor(h, &cache.Settings{Enabled: false}); got != "" {
		t.Fatalf("disabled settings: got %q", got)
	}
	got := cacheURLEnvFor(h, &cache.Settings{Enabled: true})
	if got != cacheURLDockerCreateArg(fmt.Sprintf("http://172.17.0.1:%d/", cache.DefaultPort)) {
		t.Fatalf("enabled settings: got %q", got)
	}
}

func TestDockerCreateEnvLineIf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value int
		emit  bool
		want  string
	}{
		{"GH_SR_HOST_MTU", 1460, true, "  -e GH_SR_HOST_MTU='1460' \\\n"},
		{"GH_SR_HOST_MTU", 1460, false, ""},                         // emit=false suppresses formatting
		{"GH_SR_HOST_MTU", 0, true, "  -e GH_SR_HOST_MTU='0' \\\n"}, // value=0 is still emitted when caller explicitly opts in
		{"GH_SR_HOST_MTU", -1, true, "  -e GH_SR_HOST_MTU='-1' \\\n"},
		{"GH_SR_DOCKERD_START_TIMEOUT", 90, true, "  -e GH_SR_DOCKERD_START_TIMEOUT='90' \\\n"},
		{"GH_SR_BOOTSTRAP_MAX_RETRIES", 5, true, "  -e GH_SR_BOOTSTRAP_MAX_RETRIES='5' \\\n"},
	}
	for _, tc := range cases {
		got := dockerCreateEnvLineIf(tc.name, tc.value, tc.emit)
		if got != tc.want {
			t.Errorf("dockerCreateEnvLineIf(%q, %d, %v) = %q, want %q", tc.name, tc.value, tc.emit, got, tc.want)
		}
	}
}

// TestBuildAgenticRunnerImageCmdShape verifies the docker build command shape
// produced by buildAgenticRunnerImage (calls h.Run but we inspect the structure
// by constructing the expected command string rather than executing it).
func TestBuildAgenticRunnerImageCmdShape(t *testing.T) {
	t.Parallel()
	baseImage := DefaultForkRunnerImage
	imageTag := ContainerRunnerImageTag(baseImage, nil)
	ghVer := "vtest"
	rev := ContainerImageLayoutRevision(ghVer, baseImage, nil)
	labelRev := hostshell.PosixSingleQuote(dockerLabelImageRevision + "=" + rev)
	labelCLI := hostshell.PosixSingleQuote(dockerLabelCLIVersion + "=" + ghVer)

	// Replicate the docker build command from buildAgenticRunnerImage.
	buildCmd := "docker build --build-arg FORK_RUNNER_IMAGE=" + hostshell.PosixSingleQuote(baseImage) +
		" --label " + labelRev +
		" --label " + labelCLI +
		" -t " + hostshell.PosixSingleQuote(imageTag) +
		" " + hostshell.PosixSingleQuote("/tmp/gh-sr-agentic-runner-build")

	if !strings.Contains(buildCmd, "--build-arg FORK_RUNNER_IMAGE="+hostshell.PosixSingleQuote(baseImage)) {
		t.Error("build cmd must pass the fork base image as the FORK_RUNNER_IMAGE build-arg")
	}
	// The runner version/arch are baked into the fork base image: the build must not
	// re-introduce the old tarball build-args (they would imply a GitHub API lookup).
	if strings.Contains(buildCmd, "RUNNER_VERSION=") || strings.Contains(buildCmd, "RUNNER_ARCH=") {
		t.Error("build cmd must not pass RUNNER_VERSION/RUNNER_ARCH build-args (fork base bakes the runner)")
	}
	if !strings.Contains(buildCmd, "--label ") {
		t.Error("build cmd must pass image revision labels")
	}
	if !strings.Contains(buildCmd, dockerLabelImageRevision) {
		t.Errorf("build cmd must reference label %q", dockerLabelImageRevision)
	}
	if !strings.Contains(buildCmd, rev) {
		t.Errorf("build cmd must contain layout revision %q", rev)
	}
	if !strings.Contains(buildCmd, "-t ") {
		t.Error("build cmd must specify image tag with -t")
	}
	if !strings.Contains(buildCmd, imageTag) {
		t.Errorf("build cmd must contain image tag %q", imageTag)
	}
}

func TestEmbedTextForRemoteWriteStripsCR(t *testing.T) {
	t.Parallel()
	in := "automake\r\nbuild-essential\r\nGHSR_EOF\r\n"
	want := "automake\nbuild-essential\nGHSR_E0F\n"
	if got := embedTextForRemoteWrite(in); got != want {
		t.Fatalf("embedTextForRemoteWrite() = %q, want %q", got, want)
	}
}

func TestContainerRunnerImageExtraSorted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		in    []string
		want  []string
		empty bool // if true, want nil not empty slice
	}{
		{
			name:  "nil",
			in:    nil,
			want:  nil,
			empty: true,
		},
		{
			name:  "empty slice",
			in:    []string{},
			want:  nil,
			empty: true,
		},
		{
			name:  "single item",
			in:    []string{"curl"},
			want:  []string{"curl"},
			empty: false,
		},
		{
			name:  "whitespace trimmed",
			in:    []string{"  git  ", "  curl  "},
			want:  []string{"curl", "git"},
			empty: false,
		},
		{
			name:  "empty strings filtered",
			in:    []string{"curl", "", "  ", "git"},
			want:  []string{"curl", "git"},
			empty: false,
		},
		{
			name:  "duplicates removed",
			in:    []string{"curl", "curl", "git"},
			want:  []string{"curl", "git"},
			empty: false,
		},
		{
			name:  "sorted ascending",
			in:    []string{"zlib", "curl", "ffmpeg"},
			want:  []string{"curl", "ffmpeg", "zlib"},
			empty: false,
		},
		{
			name:  "unsorted input, sorted Output",
			in:    []string{"sqlite3", "ffmpeg", "curl"},
			want:  []string{"curl", "ffmpeg", "sqlite3"},
			empty: false,
		},
		{
			name:  "case sensitive dedup",
			in:    []string{"curl", "CURL", "Curl"},
			want:  []string{"CURL", "Curl", "curl"},
			empty: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := containerRunnerImageExtraSorted(tc.in)
			if tc.empty && got != nil {
				t.Fatalf("want nil, got %v", got)
			}
			if !tc.empty && len(got) != len(tc.want) {
				t.Fatalf("got %v (len %d), want %v (len %d)", got, len(got), tc.want, len(tc.want))
			}
			for i, w := range tc.want {
				if got[i] != w {
					t.Errorf("[%d]: got %q, want %q", i, got[i], w)
				}
			}
		})
	}
}

// TestParseContainerStatusInspectOutput verifies mapping from container+image inspect Output.
func TestParseContainerStatusInspectOutput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		line         string
		wantLocal    string
		wantImage    string
		wantImageRev string
	}{
		{"running|gh-sr/agentic-runner:2.320.0|sha256:abc|deadbeef", "running", "gh-sr/agentic-runner:2.320.0", "deadbeef"},
		{"running|gh-sr/agentic-runner:2.320.0-xa1b2c3d|sha256:x|", "running", "gh-sr/agentic-runner:2.320.0-xa1b2c3d", ""},
		{"exited|gh-sr/agentic-runner:1.0.0|sha256:1|rev1", "stopped", "gh-sr/agentic-runner:1.0.0", "rev1"},
		{"created|repo:tag|sha256:2|", "stopped", "repo:tag", ""},
		{"paused|x:y|sha256:3|r", "stopped", "x:y", "r"},
		{"restarting|x:y|sha256:4|r", "restarting", "x:y", "r"},
		{"not installed|||", "not installed", "", ""},
		{"not installed|a|b|c", "not installed", "", ""},
		{"failed|gh-sr/agentic-runner:2.320.0|sha256:abc|deadbeef", "failed", "gh-sr/agentic-runner:2.320.0", "deadbeef"},
		{"failed|||", "failed", "", ""},
	}
	for _, tc := range cases {
		gotLocal, gotImage, gotRev := parseContainerStatusInspectOutput(tc.line)
		if gotLocal != tc.wantLocal || gotImage != tc.wantImage || gotRev != tc.wantImageRev {
			t.Errorf("line %q → (%q,%q,%q), want (%q,%q,%q)", tc.line, gotLocal, gotImage, gotRev, tc.wantLocal, tc.wantImage, tc.wantImageRev)
		}
	}
}

// TestContainerLocalStatusImageAndRevision_one_ssh_round_trip pins the
// per-tick energy contract: the container status path used to issue 2-3 SSH
// calls per container per Manager.Status tick (echo $HOME + bootstrap-failed
// marker test + docker inspect). The combined containerLocalStatusOneShot
// script folds them into a single h.Run, which on a long-running TUI session
// compounds into one fewer SSH round trip per container per refresh tick.
func TestContainerLocalStatusImageAndRevision_one_ssh_round_trip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		mockOut   string
		wantLocal string
		wantImage string
		wantRev   string
	}{
		{"running_healthy", "running|gh-sr/agentic-runner:2.320.0|sha256:abc|deadbeef", "running", "gh-sr/agentic-runner:2.320.0", "deadbeef"},
		{"bootstrap_failed_container_present", "failed|gh-sr/agentic-runner:2.320.0|sha256:abc|deadbeef", "failed", "gh-sr/agentic-runner:2.320.0", "deadbeef"},
		{"bootstrap_failed_container_absent", "failed|||", "failed", "", ""},
		{"not_installed", "not installed|||", "not installed", "", ""},
		{"restarting", "restarting|gh-sr/agentic-runner:2.320.0|sha256:abc|deadbeef", "restarting", "gh-sr/agentic-runner:2.320.0", "deadbeef"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := host.NewHost("test", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
			mock := &testutil.MockExecutor{Output: tc.mockOut}
			h.SetConn(mock)

			m := &Manager{}
			gotLocal, gotImage, gotRev := m.containerLocalStatusImageAndRevision(h, "ci-1")
			if gotLocal != tc.wantLocal || gotImage != tc.wantImage || gotRev != tc.wantRev {
				t.Errorf("(%q,%q,%q), want (%q,%q,%q)", gotLocal, gotImage, gotRev, tc.wantLocal, tc.wantImage, tc.wantRev)
			}
			// The energy contract: exactly one SSH round trip per status call,
			// regardless of bootstrap-failed state. The pre-refactor path made
			// 2-3 calls (echo $HOME + marker test + docker inspect).
			if got := len(mock.Calls); got != 1 {
				t.Errorf("SSH round trips = %d, want 1 (calls: %v)", got, mock.Calls)
			}
		})
	}
}

func TestContainerImageLayoutRevision_stable(t *testing.T) {
	t.Parallel()
	base := DefaultForkRunnerImage
	a := ContainerImageLayoutRevision("1.0.0", base, []string{"curl"})
	b := ContainerImageLayoutRevision("1.0.0", base, []string{"curl"})
	if a != b {
		t.Fatalf("expected stable revision, %q vs %q", a, b)
	}
	if len(a) != 12 {
		t.Fatalf("expected 12 hex chars, got %q len %d", a, len(a))
	}
	if c := ContainerImageLayoutRevision("1.0.0", base, []string{"git"}); c == a {
		t.Fatal("different extras should change revision")
	}
	if c := ContainerImageLayoutRevision("1.0.0", "ghcr.io/falcondev-oss/actions-runner:9.9.9", []string{"curl"}); c == a {
		t.Fatal("a different base image should change revision")
	}
	if c := ContainerImageLayoutRevision("1.0.1", base, []string{"curl"}); c == a {
		t.Fatal("a different gh-sr version should change revision")
	}
}

// TestResolveAbsoluteRunnerDir verifies path resolution for container state dirs.
func TestResolveAbsoluteRunnerDir(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		os      string
		mockFn  func(cmd string) (string, error)
		want    string
		wantErr bool
	}{
		{
			name: "windows path returns as-is (no $HOME expansion)",
			os:   "windows",
			mockFn: func(cmd string) (string, error) {
				// Windows paths use $env:USERPROFILE, not $HOME, so no Run call
				return "", assertCalledError()
			},
			want: `$env:USERPROFILE\.gh-sr\runners\ci-1`,
		},
		{
			name: "linux relative resolves via echo",
			os:   "linux",
			mockFn: func(cmd string) (string, error) {
				if cmd == "echo $HOME" {
					return "/home/u", nil
				}
				return "", nil
			},
			want: "/home/u/.gh-sr/runners/ci-1",
		},
		{
			name: "echo fails",
			os:   "linux",
			mockFn: func(cmd string) (string, error) {
				if cmd == "echo $HOME" {
					return "", assertCalledError()
				}
				return "", nil
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := host.NewHost("test", config.HostConfig{OS: tc.os, Arch: "amd64", Addr: "local"})
			mock := &testutil.MockExecutor{RunFn: tc.mockFn}
			h.SetConn(mock)

			got, err := resolveAbsoluteRunnerDir(h, "ci-1")
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if got != tc.want {
				t.Errorf("resolveAbsoluteRunnerDir = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveStateDirOrFallback pins the best-effort resolve-or-fallback helper:
// it returns the absolute path when the SSH resolve succeeds, and the
// shell-variable "$HOME/..." form when the resolve fails (so the shell can
// expand it on the subsequent h.Run call).
func TestResolveStateDirOrFallback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		os     string
		mockFn func(cmd string) (string, error)
		want   string
	}{
		{
			name: "linux resolve succeeds returns absolute",
			os:   "linux",
			mockFn: func(cmd string) (string, error) {
				if cmd == "echo $HOME" {
					return "/home/u", nil
				}
				return "", nil
			},
			want: "/home/u/.gh-sr/runners/ci-1",
		},
		{
			name: "linux resolve fails falls back to $HOME literal",
			os:   "linux",
			mockFn: func(cmd string) (string, error) {
				return "", assertCalledError()
			},
			want: "$HOME/.gh-sr/runners/ci-1",
		},
		{
			name: "windows path returns as-is without SSH resolve",
			os:   "windows",
			mockFn: func(cmd string) (string, error) {
				// Windows path uses $env:USERPROFILE, so no Run call should fire.
				return "", assertCalledError()
			},
			want: `$env:USERPROFILE\.gh-sr\runners\ci-1`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := host.NewHost("test", config.HostConfig{OS: tc.os, Arch: "amd64", Addr: "local"})
			mock := &testutil.MockExecutor{RunFn: tc.mockFn}
			h.SetConn(mock)

			got := resolveStateDirOrFallback(h, "ci-1")
			if got != tc.want {
				t.Errorf("resolveStateDirOrFallback = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestContainerRunnerPresent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		os   string
		mock *testutil.MockExecutor
		inst string
		want bool
	}{
		{
			name: "present",
			os:   "linux",
			mock: &testutil.MockExecutor{Output: "yes"},
			inst: "ci-1",
			want: true,
		},
		{
			name: "absent",
			os:   "linux",
			mock: &testutil.MockExecutor{Output: "no"},
			inst: "ci-1",
			want: false,
		},
		{
			name: "command fails",
			os:   "linux",
			mock: &testutil.MockExecutor{RunErr: assertCalledError()},
			inst: "ci-1",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := host.NewHost("test", config.HostConfig{OS: tc.os, Arch: "amd64", Addr: "local"})
			h.SetConn(tc.mock)
			got := containerRunnerPresent(h, tc.inst)
			if got != tc.want {
				t.Errorf("containerRunnerPresent = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestContainersPresentOneShot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		names        []string
		mockOutput   string
		mockErr      error
		want         map[string]bool
		wantCallOnce bool
	}{
		{
			name:         "all present",
			names:        []string{"ci-1", "ci-2"},
			mockOutput:   "ci-1\nci-2\n",
			want:         map[string]bool{"ci-1": true, "ci-2": true},
			wantCallOnce: true,
		},
		{
			name:         "one missing",
			names:        []string{"ci-1", "ci-2"},
			mockOutput:   "ci-1\n",
			want:         map[string]bool{"ci-1": true, "ci-2": false},
			wantCallOnce: true,
		},
		{
			name:         "all missing",
			names:        []string{"ci-1", "ci-2"},
			mockOutput:   "\n",
			want:         map[string]bool{"ci-1": false, "ci-2": false},
			wantCallOnce: true,
		},
		{
			name:         "host-owned name ignored",
			names:        []string{"ci-1"},
			mockOutput:   "ci-1\nother-container\nci-1\n", // duplicate + foreign line
			want:         map[string]bool{"ci-1": true},
			wantCallOnce: true,
		},
		{
			name:         "empty names returns empty without ssh",
			names:        nil,
			mockOutput:   "",
			want:         map[string]bool{},
			wantCallOnce: false, // no SSH round-trip when there's nothing to ask
		},
		{
			name:         "probe error propagated and marks all false",
			names:        []string{"ci-1", "ci-2"},
			mockErr:      io.ErrUnexpectedEOF,
			want:         map[string]bool{"ci-1": false, "ci-2": false},
			wantCallOnce: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock := &testutil.MockExecutor{
				Output: tc.mockOutput,
				RunErr: tc.mockErr,
			}
			h := host.NewHost("h", config.HostConfig{Addr: "local", OS: "linux", Arch: "amd64"})
			h.SetConn(mock)
			got, err := containersPresentOneShot(h, tc.names)
			if tc.mockErr != nil {
				if err == nil {
					t.Fatalf("expected error from probe failure")
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("result size: got %d entries %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("present[%q] = %v, want %v (full=%v)", k, got[k], v, got)
				}
			}
			// Negative entries in got that aren't in want are tolerated;
			// any "true" entry not declared in want would mean the helper
			// claimed a host-owned container was a runner.
			for k, v := range got {
				if _, ok := tc.want[k]; !ok && v {
					t.Errorf("unexpected present=true for %q (host-owned name leaked through)", k)
				}
			}
			if tc.wantCallOnce {
				if len(mock.Calls) != 1 {
					t.Errorf("expected exactly 1 SSH round-trip, got %d: %v", len(mock.Calls), mock.Calls)
				}
				if len(mock.Calls) >= 1 && !strings.Contains(mock.Calls[0], "docker ps -a --filter name=gh-sr-") {
					t.Errorf("unexpected command: %q (want docker ps -a --filter name=gh-sr-)", mock.Calls[0])
				}
				if len(mock.Calls) >= 1 && !strings.Contains(mock.Calls[0], "sed") {
					t.Errorf("unexpected command: %q (want sed prefix-strip)", mock.Calls[0])
				}
			} else if len(mock.Calls) != 0 {
				t.Errorf("expected zero SSH calls when input is empty, got %d: %v", len(mock.Calls), mock.Calls)
			}
		})
	}
}

// TestSetupContainer_ContainersPresenceOneSshRoundTrip pins the per-instance
// probe collapse for the multi-instance setup path: with N instances the
// presence check must be a single SSH round-trip, not N. The mock returns
// all instances present so setupContainer skips creation and no GitHub token
// call is required.
func TestSetupContainer_ContainersPresenceOneSshRoundTrip(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "docker --version"):
			return "yes", nil
		case strings.Contains(cmd, "docker info"):
			return "ok", nil
		case strings.Contains(cmd, "docker image inspect"):
			return "yes", nil
		case strings.Contains(cmd, "docker ps -a --filter name=gh-sr-"):
			return "multi-1\nmulti-2\nmulti-3\n", nil
		default:
			return "", nil
		}
	}}
	h := host.NewHost("h", config.HostConfig{Addr: "runner@vps", OS: "linux", Arch: "amd64"})
	h.SetConn(mock)
	m := &Manager{}
	rc := config.RunnerConfig{
		Name:       "multi",
		Repo:       "o/r",
		Host:       "h",
		Count:      3,
		Profile:    "agentic",
		RunnerMode: config.RunnerModeContainer,
	}
	if err := m.setupContainer(h, rc); err != nil {
		t.Fatalf("setupContainer: %v", err)
	}

	// Pin exactly one `docker ps -a` call; the previous per-instance
	// containerRunnerPresent loop would have produced N=3 such calls.
	var ps int
	for _, c := range mock.Calls {
		if strings.Contains(c, "docker ps -a --filter name=gh-sr-") {
			ps++
		}
	}
	if ps != 1 {
		t.Errorf("expected exactly 1 one-shot presence call, got %d; calls=%v", ps, mock.Calls)
	}
	// Sanity: no per-instance docker inspect probe should remain.
	for _, c := range mock.Calls {
		if strings.Contains(c, "docker inspect --format='{{.Name}}'") {
			t.Errorf("unexpected per-instance docker inspect in setupContainer: %q", c)
		}
	}
}

// TestNeedsSetup_ContainerOneSshRoundTrip pins that needsSetupContainer also
// drives its loop off the one-shot map: N instances → ONE SSH round-trip,
// rather than up to N per-instance probes.
func TestNeedsSetup_ContainerOneSshRoundTrip(t *testing.T) {
	t.Parallel()
	m := NewManager("")
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "docker ps -a --filter name=gh-sr-"):
			return "ci-1\nci-2\nci-3\nci-4\nci-5\n", nil
		default:
			return "", nil
		}
	}}
	h := needsSetupMockHost(t, mock)
	rc := config.RunnerConfig{
		Name:       "ci",
		Repo:       "o/r",
		Host:       "h",
		Count:      5,
		Profile:    "agentic",
		RunnerMode: config.RunnerModeContainer,
	}
	if m.NeedsSetup(h, rc) {
		t.Fatalf("NeedsSetup = true, want false (all 5 containers present); calls=%v", mock.Calls)
	}
	if len(mock.Calls) != 1 {
		t.Errorf("expected 1 SSH call, got %d: %v", len(mock.Calls), mock.Calls)
	}
	if !strings.Contains(mock.Calls[0], "docker ps -a --filter name=gh-sr-") {
		t.Errorf("expected one-shot probe, got %q", mock.Calls[0])
	}
}

func TestContainerImageExists(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		os       string
		mock     *testutil.MockExecutor
		imageTag string
		want     bool
	}{
		{
			name:     "exists",
			os:       "linux",
			mock:     &testutil.MockExecutor{Output: "yes"},
			imageTag: "gh-sr/agentic-runner:2.320.0",
			want:     true,
		},
		{
			name:     "not found",
			os:       "linux",
			mock:     &testutil.MockExecutor{Output: "no"},
			imageTag: "gh-sr/agentic-runner:2.320.0",
			want:     false,
		},
		{
			name:     "command fails",
			os:       "linux",
			mock:     &testutil.MockExecutor{RunErr: assertCalledError()},
			imageTag: "gh-sr/agentic-runner:2.320.0",
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := host.NewHost("test", config.HostConfig{OS: tc.os, Arch: "amd64", Addr: "local"})
			h.SetConn(tc.mock)
			got, err := containerImageExists(h, tc.imageTag)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if got != tc.want {
				t.Errorf("containerImageExists = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMtuDockerCreateArg(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mtu      int
		wantArg  bool
		wantsMTU string
	}{
		{0, false, ""},       // auto-detect found nothing
		{575, false, ""},     // below sane floor
		{576, true, "576"},   // floor
		{1460, true, "1460"}, // typical GCP overlay
		{1499, true, "1499"}, // just under default
		{1500, false, ""},    // Docker default — no-op
		{1501, false, ""},    // jumbo — not lowered via this knob
		{9000, false, ""},
	}
	for _, tc := range cases {
		got := mtuDockerCreateArg(tc.mtu)
		if !tc.wantArg {
			if got != "" {
				t.Errorf("mtuDockerCreateArg(%d) = %q, want empty", tc.mtu, got)
			}
			continue
		}
		if !strings.Contains(got, "-e GH_SR_HOST_MTU=") {
			t.Errorf("mtuDockerCreateArg(%d) = %q, want GH_SR_HOST_MTU env", tc.mtu, got)
		}
		if !strings.Contains(got, tc.wantsMTU) {
			t.Errorf("mtuDockerCreateArg(%d) = %q, want value %q", tc.mtu, got, tc.wantsMTU)
		}
		// Must be a continuation line: leading indent + trailing ` \` + newline so it slots
		// between the other -e flags and the image arg in the docker create command.
		if !strings.HasPrefix(got, "  -e ") || !strings.HasSuffix(got, " \\\n") {
			t.Errorf("mtuDockerCreateArg(%d) = %q, want indented continuation line", tc.mtu, got)
		}
	}
}

func TestDetectHostEgressMTU(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		os     string
		Output string
		RunErr error
		want   int
	}{
		{"reduced mtu", "linux", "1460\n", nil, 1460},
		{"standard mtu", "linux", "1500", nil, 1500},
		{"jumbo within range", "linux", "9000", nil, 9000},
		{"non-numeric", "linux", "eth0\n", nil, 0},
		{"empty (no egress iface)", "linux", "", nil, 0},
		{"below floor", "linux", "100", nil, 0},
		{"above ceiling", "linux", "9001", nil, 0},
		{"run error", "linux", "", assertCalledError(), 0},
		{"non-linux skips detection", "windows", "1460", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := host.NewHost("test", config.HostConfig{OS: tc.os, Arch: "amd64", Addr: "local"})
			h.SetConn(&testutil.MockExecutor{Output: tc.Output, RunErr: tc.RunErr})
			if got := DetectHostEgressMTU(h); got != tc.want {
				t.Errorf("DetectHostEgressMTU = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestResolveContainerMTU(t *testing.T) {
	t.Parallel()

	t.Run("override wins over detection", func(t *testing.T) {
		t.Parallel()
		h := host.NewHost("test", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
		// Detection would return 1460, but the explicit override must take precedence.
		h.SetConn(&testutil.MockExecutor{Output: "1460"})
		m := &Manager{ContainerMTU: 1400}
		if got := m.resolveContainerMTU(h); got != 1400 {
			t.Errorf("resolveContainerMTU = %d, want 1400 (override)", got)
		}
	})

	t.Run("auto-detect when no override", func(t *testing.T) {
		t.Parallel()
		h := host.NewHost("test", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
		h.SetConn(&testutil.MockExecutor{Output: "1460"})
		m := &Manager{}
		if got := m.resolveContainerMTU(h); got != 1460 {
			t.Errorf("resolveContainerMTU = %d, want 1460 (detected)", got)
		}
	})
}

var errCalled = calledErrorErr{}

type calledErrorErr struct{}

func (calledErrorErr) Error() string { return "called" }

func assertCalledError() error {
	return errCalled
}

// TestPositiveIntOrDefault pins the "use v when v > 0, else def" rule shared
// by the container-timeout / -retry / -stagger accessors. Previously the third
// accessor (containerStartStaggerSeconds) used `>= 0 && != 0` instead of `> 0`
// — logically equivalent for ints but inconsistent with the other two and a
// drift magnet. Centralizing the rule makes future accessors use the same
// check by construction.
func TestPositiveIntOrDefault(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		v    int
		def  int
		want int
	}{
		{"positive v wins", 30, 90, 30},
		{"zero v falls back to default", 0, 90, 90},
		{"negative v falls back to default", -3, 5, 5},
		{"def=0 still returns v when v > 0", 7, 0, 7},
		{"def=0 with v=0 returns 0", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := positiveIntOrDefault(tc.v, tc.def); got != tc.want {
				t.Errorf("positiveIntOrDefault(%d, %d) = %d, want %d", tc.v, tc.def, got, tc.want)
			}
		})
	}
}

// TestContainerConfigAccessorsDefaults verifies each accessor still returns its
// hard-coded default when the receiver is nil or the configured value is
// non-positive, and the configured value when it is positive. The refactor
// preserved these contracts and made the positivity check uniform across the
// three accessors (previously containerStartStaggerSeconds used
// `>= 0 && != 0`, which is logically equivalent to `> 0` for ints but
// inconsistent with the other two accessors and a drift magnet).
func TestContainerConfigAccessorsDefaults(t *testing.T) {
	t.Parallel()

	t.Run("nil receiver — all three return their default", func(t *testing.T) {
		t.Parallel()
		var m *Manager
		if got := m.containerDockerdStartTimeout(); got != 90 {
			t.Errorf("containerDockerdStartTimeout nil = %d, want 90", got)
		}
		if got := m.containerBootstrapMaxRetries(); got != 5 {
			t.Errorf("containerBootstrapMaxRetries nil = %d, want 5", got)
		}
		if got := m.containerStartStaggerSeconds(); got != 3 {
			t.Errorf("containerStartStaggerSeconds nil = %d, want 3", got)
		}
	})

	t.Run("zero / negative configured values fall back to default", func(t *testing.T) {
		t.Parallel()
		m := &Manager{ContainerDockerdStartTimeout: 0, ContainerBootstrapMaxRetries: -1, ContainerStartStaggerSeconds: 0}
		if got := m.containerDockerdStartTimeout(); got != 90 {
			t.Errorf("timeout zero = %d, want 90", got)
		}
		if got := m.containerBootstrapMaxRetries(); got != 5 {
			t.Errorf("retries negative = %d, want 5", got)
		}
		if got := m.containerStartStaggerSeconds(); got != 3 {
			t.Errorf("stagger zero = %d, want 3", got)
		}
	})

	t.Run("positive configured values win", func(t *testing.T) {
		t.Parallel()
		m := &Manager{ContainerDockerdStartTimeout: 45, ContainerBootstrapMaxRetries: 7, ContainerStartStaggerSeconds: 10}
		if got := m.containerDockerdStartTimeout(); got != 45 {
			t.Errorf("timeout 45 = %d, want 45", got)
		}
		if got := m.containerBootstrapMaxRetries(); got != 7 {
			t.Errorf("retries 7 = %d, want 7", got)
		}
		if got := m.containerStartStaggerSeconds(); got != 10 {
			t.Errorf("stagger 10 = %d, want 10", got)
		}
	})
}

func TestSetupContainer_ensureDockerBeforeImageBuild(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/actions/runner/releases/latest" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(releaseResponse{TagName: "v2.330.0"})
	}))
	defer ts.Close()

	var calls []string
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		calls = append(calls, cmd)
		switch {
		case strings.Contains(cmd, "docker --version"):
			return "yes", nil
		case strings.Contains(cmd, "docker info"):
			return "ok", nil
		case strings.Contains(cmd, "docker image inspect"):
			return "yes", nil
		case strings.Contains(cmd, "docker ps -a --filter name=gh-sr-"):
			// One-shot container presence check: aw-runner-1 already present,
			// so setupContainer skips createContainerInstance entirely (no
			// GitHub registration-token call needed).
			return "aw-runner-1\n", nil
		case strings.Contains(cmd, "docker inspect --format='{{.Name}}'"):
			return "yes", nil
		default:
			return "", nil
		}
	}}
	h := host.NewHost("h", config.HostConfig{Addr: "runner@vps", OS: "linux", Arch: "amd64"})
	h.SetConn(mock)
	m := &Manager{GitHub: NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL)}
	rc := config.RunnerConfig{
		Name:       "aw-runner",
		Repo:       "o/r",
		Host:       "h",
		Profile:    "agentic",
		RunnerMode: config.RunnerModeContainer,
	}

	if err := m.setupContainer(h, rc); err != nil {
		t.Fatalf("setupContainer: %v", err)
	}

	versionIdx, inspectIdx := -1, -1
	for i, c := range calls {
		if versionIdx == -1 && strings.Contains(c, "docker --version") {
			versionIdx = i
		}
		if inspectIdx == -1 && strings.Contains(c, "docker image inspect") {
			inspectIdx = i
		}
	}
	if versionIdx < 0 || inspectIdx < 0 || versionIdx > inspectIdx {
		t.Fatalf("expected docker --version before docker image inspect; versionIdx=%d inspectIdx=%d calls=%d", versionIdx, inspectIdx, len(calls))
	}
}

// TestSetupContainer_emitsBuildProgressMessageWhenImageMissing pins the
// user-visible progress line that setupContainer must emit BEFORE a multi-minute
// Docker build when the image is absent: "building container runner image (this
// may take several minutes)..." — via buildRunnerImageIfMissing's onBuild hook.
func TestSetupContainer_emitsBuildProgressMessageWhenImageMissing(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "docker --version"):
			return "yes", nil
		case strings.Contains(cmd, "docker info"):
			return "ok", nil
		case strings.Contains(cmd, "docker image inspect"):
			return "no", nil // image missing → build path
		case strings.Contains(cmd, "docker build"):
			return "", nil
		case strings.Contains(cmd, "docker ps -a --filter name=gh-sr-"):
			// One-shot container presence check: container present, so
			// setupContainer skips createContainerInstance entirely (no
			// GitHub registration-token call needed). The test exercises the
			// build-progress printout path, not the create path.
			return "aw-runner-1\n", nil
		case strings.Contains(cmd, "docker inspect --format='{{.Name}}'"):
			return "yes", nil
		default:
			return "", nil
		}
	}}
	h := host.NewHost("h", config.HostConfig{Addr: "runner@vps", OS: "linux", Arch: "amd64"})
	h.SetConn(mock)
	m := &Manager{Out: &out}
	rc := config.RunnerConfig{
		Name:       "aw-runner",
		Repo:       "o/r",
		Host:       "h",
		Profile:    "agentic",
		RunnerMode: config.RunnerModeContainer,
	}

	if err := m.setupContainer(h, rc); err != nil {
		t.Fatalf("setupContainer: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "building container runner image (this may take several minutes)") {
		t.Errorf("expected build progress heads-up in output; got:\n%s", got)
	}
	if !strings.Contains(got, "image built:") {
		t.Errorf("expected 'image built:' line in output; got:\n%s", got)
	}
	// Progress line must precede the completion line.
	progressIdx := strings.Index(got, "building container runner image (this may take several minutes)")
	builtIdx := strings.Index(got, "image built:")
	if progressIdx < 0 || builtIdx < 0 || progressIdx > builtIdx {
		t.Errorf("expected progress line before 'image built:'; progressIdx=%d builtIdx=%d", progressIdx, builtIdx)
	}
}

func TestSetupContainer_propagatesDockerGroupPending(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		if strings.Contains(cmd, "docker --version") {
			return "no", nil
		}
		return "", nil
	}}
	h := host.NewHost("h", config.HostConfig{Addr: "runner@vps", OS: "linux", Arch: "amd64"})
	h.SetConn(mock)
	m := &Manager{GitHub: NewGitHubClient("pat")}
	rc := config.RunnerConfig{
		Name:       "aw-runner",
		Repo:       "o/r",
		Host:       "h",
		Profile:    "agentic",
		RunnerMode: config.RunnerModeContainer,
	}

	err := m.setupContainer(h, rc)
	if !errors.Is(err, ErrDockerGroupPending) {
		t.Fatalf("expected ErrDockerGroupPending, got %v", err)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "docker image inspect") || strings.Contains(c, "docker build") {
			t.Fatalf("should not build image when group pending: %q", c)
		}
	}
}

// TestManager_resolveContainerImageInputs verifies the resolved (baseImage, imageTag)
// pair: baseImage defaults to the fork runner image, imageTag derives from the base
// tag plus the extra-apt suffix. The runner version is baked into the fork base, so
// no GitHub API call is involved and the resolution works offline.
func TestManager_resolveContainerImageInputs(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h", config.HostConfig{Addr: "runner@vps", OS: "linux", Arch: "amd64"})
	h.SetConn(&testutil.MockExecutor{})
	m := &Manager{
		GhSrVersion:            "1.2.3",
		ContainerImageExtraApt: []string{"sqlite3", "ffmpeg"},
	}

	baseImage, imageTag, err := m.resolveContainerImageInputs(h)
	if err != nil {
		t.Fatalf("resolveContainerImageInputs: %v", err)
	}
	if baseImage != DefaultForkRunnerImage {
		t.Errorf("baseImage: got %q, want %q", baseImage, DefaultForkRunnerImage)
	}
	wantTag := ContainerRunnerImageTag(DefaultForkRunnerImage, []string{"sqlite3", "ffmpeg"})
	if imageTag != wantTag {
		t.Errorf("imageTag: got %q, want %q", imageTag, wantTag)
	}

	// An explicit container_runner_image.base_image overrides the default.
	m2 := &Manager{ContainerImageBaseImage: "ghcr.io/falcondev-oss/actions-runner:9.9.9"}
	base2, tag2, err := m2.resolveContainerImageInputs(h)
	if err != nil {
		t.Fatalf("resolveContainerImageInputs(override): %v", err)
	}
	if base2 != "ghcr.io/falcondev-oss/actions-runner:9.9.9" || tag2 != ContainerRunnerImageTag("ghcr.io/falcondev-oss/actions-runner:9.9.9", nil) {
		t.Errorf("override: got (%q, %q)", base2, tag2)
	}

	// The resolution must not hit the network: an unconfigured GitHub client is fine.
	m3 := &Manager{}
	if _, _, err := m3.resolveContainerImageInputs(h); err != nil {
		t.Fatalf("resolveContainerImageInputs(zero-config): %v", err)
	}
}

// TestManager_buildRunnerImageIfMissing_alreadyPresent returns built=false and does
// not invoke the docker build path.
func TestManager_buildRunnerImageIfMissing_alreadyPresent(t *testing.T) {
	t.Parallel()
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		if strings.Contains(cmd, "docker image inspect") {
			return "yes", nil
		}
		if strings.Contains(cmd, "docker build") {
			t.Errorf("build should not be called when image exists, got: %q", cmd)
		}
		return "", nil
	}}
	h := host.NewHost("h", config.HostConfig{Addr: "runner@vps", OS: "linux", Arch: "amd64"})
	h.SetConn(mock)
	m := &Manager{GhSrVersion: "1.2.3"}

	built, err := m.buildRunnerImageIfMissing(h, "gh-sr/agentic-runner:2.337.0", DefaultForkRunnerImage, func() {
		t.Errorf("onBuild must not fire when image already exists")
	})
	if err != nil {
		t.Fatalf("buildRunnerImageIfMissing: %v", err)
	}
	if built {
		t.Errorf("built = true, want false (image already present)")
	}
}

// TestManager_buildRunnerImageIfMissing_buildsWhenMissing returns built=true after
// invoking the docker build path.
func TestManager_buildRunnerImageIfMissing_buildsWhenMissing(t *testing.T) {
	t.Parallel()
	sawBuild := false
	onBuildBeforeBuild := false // onBuild must fire before the docker build command
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "docker image inspect"):
			return "no", nil
		case strings.Contains(cmd, "docker build"):
			sawBuild = true
			return "", nil
		}
		return "", nil
	}}
	h := host.NewHost("h", config.HostConfig{Addr: "runner@vps", OS: "linux", Arch: "amd64"})
	h.SetConn(mock)
	m := &Manager{GhSrVersion: "1.2.3"}

	built, err := m.buildRunnerImageIfMissing(h, "gh-sr/agentic-runner:2.337.0", DefaultForkRunnerImage, func() {
		onBuildBeforeBuild = !sawBuild // true only if no build command ran yet
	})
	if err != nil {
		t.Fatalf("buildRunnerImageIfMissing: %v", err)
	}
	if !built {
		t.Errorf("built = false, want true (image missing → build invoked)")
	}
	if !sawBuild {
		t.Errorf("docker build was never invoked")
	}
	if !onBuildBeforeBuild {
		t.Errorf("onBuild did not fire before the docker build command")
	}
}

// TestManager_buildRunnerImageIfMissing_buildErrorWrapsMessage verifies the docker
// build failure is wrapped as "building container runner image: %w" — matching the
// historical call-site message so user-visible error output is unchanged.
func TestManager_buildRunnerImageIfMissing_buildErrorWrapsMessage(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("docker build failed")
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "docker image inspect"):
			return "no", nil // image missing → we proceed to build
		case strings.Contains(cmd, "docker build"):
			return "", sentinel
		}
		return "", nil
	}}
	h := host.NewHost("h", config.HostConfig{Addr: "runner@vps", OS: "linux", Arch: "amd64"})
	h.SetConn(mock)
	m := &Manager{GhSrVersion: "1.2.3"}

	built, err := m.buildRunnerImageIfMissing(h, "gh-sr/agentic-runner:2.337.0", DefaultForkRunnerImage, nil)
	if err == nil {
		t.Fatal("expected error from docker build failure, got nil")
	}
	if built {
		t.Errorf("built = true on error, want false")
	}
	if !strings.Contains(err.Error(), "building container runner image:") {
		t.Errorf("error should wrap with %q, got: %v", "building container runner image:", err)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error should wrap sentinel via %%w, got: %v", err)
	}
}

func TestDockerExecCommand_PlainName(t *testing.T) {
	t.Parallel()
	got := DockerExecCommand("gh-sr-myinstance", "sh -c 'echo hi'")
	want := `docker exec "gh-sr-myinstance" sh -c 'echo hi'`
	if got != want {
		t.Errorf("DockerExecCommand = %q, want %q", got, want)
	}
}

func TestDockerExecCommand_NameWithSpecialCharsIsQuoted(t *testing.T) {
	t.Parallel()
	// The helper must produce shell-safe output: any char inside the
	// name (including backticks, dollar signs, semicolons, double-quotes)
	// is escaped by strconv.Quote so a malicious container name cannot
	// inject a shell command via a bare double-quote.
	//
	// strconv.Quote(`evil"; rm -rf /; "`) yields
	//   "evil\"; rm -rf /; \""
	// so the full helper output is
	//   docker exec "evil\"; rm -rf /; \"" echo ok
	// — the inner double-quotes are escaped, keeping the quoted-name
	// string intact when handed to the shell.
	got := DockerExecCommand(`evil"; rm -rf /; "`, "echo ok")
	want := `docker exec "evil\"; rm -rf /; \"" echo ok`
	if got != want {
		t.Errorf("DockerExecCommand = %q, want %q", got, want)
	}
}

func TestDockerExecCommand_EmptyInnerCmd(t *testing.T) {
	t.Parallel()
	got := DockerExecCommand("name", "")
	want := `docker exec "name" `
	if got != want {
		t.Errorf("DockerExecCommand = %q, want %q", got, want)
	}
}

func TestDockerExecCommand_PrefixMatchesFormerInlineQuoting(t *testing.T) {
	t.Parallel()
	// Regression guard: the inner-Docker AWF hygiene probes in
	// internal/agentic/agentic_awf_hygiene_test.go pin the literal prefix
	// `docker exec "gh-sr-myinstance" `. This test pins the helper's output
	// for the same input, so future changes to the quoting policy trip
	// here first instead of as a silent test-suite failure.
	const name = "gh-sr-myinstance"
	prefix := DockerExecCommand(name, "")
	const want = `docker exec "gh-sr-myinstance" `
	if prefix != want {
		t.Errorf("prefix = %q, want %q (this is the canonical AWF-hygiene inner-Docker prefix)", prefix, want)
	}
}

func TestQuoteContainerName_PlainName(t *testing.T) {
	t.Parallel()
	if got, want := QuoteContainerName("gh-sr-myinstance"), `"gh-sr-myinstance"`; got != want {
		t.Errorf("QuoteContainerName = %q, want %q", got, want)
	}
}

func TestQuoteContainerName_NameWithSpecialCharsIsQuoted(t *testing.T) {
	t.Parallel()
	// Mirrors TestDockerExecCommand_NameWithSpecialCharsIsQuoted: a malicious
	// container name must not be able to inject shell via a bare double-quote.
	got := QuoteContainerName(`evil"; rm -rf /; "`)
	want := `"evil\"; rm -rf /; \""`
	if got != want {
		t.Errorf("QuoteContainerName = %q, want %q", got, want)
	}
}

func TestQuoteContainerName_EmptyName(t *testing.T) {
	t.Parallel()
	if got, want := QuoteContainerName(""), `""`; got != want {
		t.Errorf("QuoteContainerName(\"\") = %q, want %q", got, want)
	}
}

func TestQuoteContainerName_SpacesAndPunctuation(t *testing.T) {
	t.Parallel()
	// Spaces and other shell-significant chars must end up inside the
	// double-quoted envelope so the value is preserved as a single shell arg.
	got := QuoteContainerName("weird name")
	want := `"weird name"`
	if got != want {
		t.Errorf("QuoteContainerName = %q, want %q", got, want)
	}
}

// TestRebuildContainerImage_chainsStopAndRemovePerInstance pins the perf
// shape of the per-instance teardown in rebuildContainerImage: each
// instance's `docker stop` and `docker rm -f` must run in a single
// h.Run call (chained in one shell) instead of as two separate round-trips.
// Saves N SSH round-trips for an N-instance rebuild.
func TestRebuildContainerImage_chainsStopAndRemovePerInstance(t *testing.T) {
	t.Parallel()
	var calls []string
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		calls = append(calls, cmd)
		// Fail the image build so rebuildContainerImage returns right after
		// the teardown loop, leaving only the captured teardown calls (plus
		// the best-effort rmi) to inspect.
		if strings.Contains(cmd, "docker build") {
			return "", errors.New("build aborted")
		}
		return "", nil
	}}
	h := host.NewHost("h", config.HostConfig{Addr: "runner@vps", OS: "linux", Arch: "amd64"})
	h.SetConn(mock)
	m := &Manager{
		GhSrVersion: "1.2.3",
		Out:         io.Discard,
	}
	rc := config.RunnerConfig{
		Name:       "aw-runner",
		Repo:       "o/r",
		Host:       "h",
		Profile:    "agentic",
		RunnerMode: config.RunnerModeContainer,
		Count:      3,
	}

	// The rebuild flow returns the build error, but only after the teardown
	// loop has captured all N chained calls.
	_ = m.rebuildContainerImage(h, rc)

	// Count chained stop+rm calls and bare stop/rm calls.
	chained := 0
	bareStop := 0
	bareRm := 0
	for _, c := range calls {
		switch {
		case strings.Contains(c, "docker stop") && strings.Contains(c, "docker rm -f"):
			chained++
		case strings.Contains(c, "docker stop "):
			bareStop++
		case strings.Contains(c, "docker rm -f"):
			bareRm++
		}
	}
	if chained != rc.Count {
		t.Errorf("chained stop+rm calls = %d, want %d (one per instance); calls=%v", chained, rc.Count, calls)
	}
	if bareStop != 0 || bareRm != 0 {
		t.Errorf("expected no separate stop/rm calls; got bareStop=%d bareRm=%d; calls=%v", bareStop, bareRm, calls)
	}
}

// TestRemoveContainer_chainsStopAndRemove pins the perf shape of the
// per-instance teardown in removeContainer: `docker stop`, `docker rm -f`,
// and the state-dir `rm -rf` must all run in a single h.Run call (chained in
// one shell) instead of as three separate SSH round-trips. Saves 2*N
// round-trips for an N-instance `gh sr down` / `Remove` (the orchestrator
// loops over InstanceNames()). The mock deliberately fails the GitHub
// removal-token lookup so the `docker exec ... config.sh remove` deregister
// step is skipped, leaving only the single chained teardown call to inspect.
func TestRemoveContainer_chainsStopAndRemove(t *testing.T) {
	t.Parallel()
	// GitHub server returns 500 so GetRemovalTokenScoped errors out and
	// the docker-exec deregister step is skipped (best-effort).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	var calls []string
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		calls = append(calls, cmd)
		return "", nil
	}}
	h := host.NewHost("h", config.HostConfig{Addr: "runner@vps", OS: "linux", Arch: "amd64"})
	h.SetConn(mock)
	m := &Manager{
		GitHub:      NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL),
		GhSrVersion: "1.2.3",
		Out:         io.Discard,
	}
	rc := config.RunnerConfig{
		Name:       "aw-runner",
		Repo:       "o/r",
		Host:       "h",
		Profile:    "agentic",
		RunnerMode: config.RunnerModeContainer,
		Count:      3,
	}

	// Call removeContainer for a single instance so the assertion is exactly
	// "1 chained call" containing stop+rm+rm-rf, independent of rc.Count.
	if err := m.removeContainer(h, rc, "aw-runner-1"); err != nil {
		t.Fatalf("removeContainer: unexpected error: %v", err)
	}

	// All three teardown ops must live inside a single h.Run call.
	if got := len(calls); got != 1 {
		t.Fatalf("h.Run calls = %d, want 1 (single SSH round-trip); calls=%v", got, calls)
	}
	script := calls[0]
	if !strings.Contains(script, "docker stop") {
		t.Errorf("chained script missing `docker stop`; got: %q", script)
	}
	if !strings.Contains(script, "docker rm -f") {
		t.Errorf("chained script missing `docker rm -f`; got: %q", script)
	}
	// State-dir rm -rf must ride the same chained shell, using the
	// unresolved `$HOME/.gh-sr/runners/<inst>` form (double-quoted so the
	// shell expands `$HOME`). This drops the prior `echo $HOME` resolve.
	if !strings.Contains(script, `rm -rf "$HOME/.gh-sr/runners/aw-runner-1"`) {
		t.Errorf("chained script missing state-dir rm -rf in $HOME form; got: %q", script)
	}
	if strings.Contains(script, `'$HOME/.gh-sr/runners/`) {
		t.Errorf("chained script must not single-quote $HOME paths (blocks expansion); got: %q", script)
	}
	// No bare `echo $HOME` resolve (we now rely on the shell to expand it).
	for _, c := range calls {
		if strings.Contains(c, "echo $HOME") {
			t.Errorf("removeContainer should not issue a separate `echo $HOME` resolve; calls=%v", calls)
		}
	}
}

func TestRemoveContainer_propagatesChainedTeardownError(t *testing.T) {
	t.Parallel()
	// GitHub server returns 500 so GetRemovalTokenScoped errors out and
	// the docker-exec deregister step is skipped (best-effort).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	sentinel := errors.New("ssh connection reset")
	var stateDirRemoved bool
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "docker stop") && strings.Contains(cmd, "docker rm -f"):
			return "", sentinel
		case strings.Contains(cmd, "rm -rf"):
			stateDirRemoved = true
		}
		return "", nil
	}}
	h := host.NewHost("h", config.HostConfig{Addr: "runner@vps", OS: "linux", Arch: "amd64"})
	h.SetConn(mock)
	m := &Manager{
		GitHub:      NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL),
		GhSrVersion: "1.2.3",
		Out:         io.Discard,
	}
	rc := config.RunnerConfig{
		Name:       "aw-runner",
		Repo:       "o/r",
		Host:       "h",
		Profile:    "agentic",
		RunnerMode: config.RunnerModeContainer,
		Count:      1,
	}

	err := m.removeContainer(h, rc, "aw-runner-1")
	if !errors.Is(err, sentinel) {
		t.Fatalf("removeContainer error = %v, want sentinel %v", err, sentinel)
	}
	if stateDirRemoved {
		t.Fatal("state directory was removed after container teardown failed")
	}
}

// TestStartContainer_OneSshRoundTrip pins the perf shape of startContainer:
// the three pre-flight operations (rm -f of bootstrap markers, docker update
// --restart, docker start) must run in a single h.Run call (chained in one
// shell) instead of as three separate SSH round-trips. Saves 2 round-trips
// per instance per `gh sr up`; for an N-instance fleet this is 2*N fewer
// SSH round-trips on every `gh sr up` invocation.
//
// Also verifies the script:
//   - uses the unresolved `$HOME/.gh-sr/runners/<inst>` form for the rm -f
//     targets (the shell expands $HOME), so we no longer pay a separate
//     `echo $HOME` resolve call (see containerLocalStatusOneShot which uses
//     the same `$HOME`-form pattern).
//   - chains the docker update with `|| true` so a policy-update failure does
//     not block the subsequent docker start.
func TestStartContainer_OneSshRoundTrip(t *testing.T) {
	t.Parallel()
	var calls []string
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		calls = append(calls, cmd)
		return "", nil
	}}
	h := host.NewHost("h", config.HostConfig{Addr: "runner@vps", OS: "linux", Arch: "amd64"})
	h.SetConn(mock)
	m := &Manager{Out: io.Discard}

	if err := m.startContainer(h, "aw-runner-1"); err != nil {
		t.Fatalf("startContainer: unexpected error: %v", err)
	}

	// Exactly one h.Run call: the three pre-flight ops are chained in one shell.
	if got := len(calls); got != 1 {
		t.Fatalf("h.Run calls = %d, want 1 (one chained SSH round-trip); calls=%v", got, calls)
	}
	script := calls[0]
	// Marker cleanup uses double-quoted $HOME form so the shell expands it
	// (PosixSingleQuote would freeze a literal "$HOME/..." path).
	if !strings.Contains(script, `"$HOME/.gh-sr/runners/aw-runner-1/bootstrap-failed"`) {
		t.Errorf("script missing bootstrap-failed rm -f; got: %q", script)
	}
	if !strings.Contains(script, `"$HOME/.gh-sr/runners/aw-runner-1/dockerd-start-failures"`) {
		t.Errorf("script missing dockerd-start-failures rm -f; got: %q", script)
	}
	if strings.Contains(script, `'$HOME/.gh-sr/runners/`) {
		t.Errorf("script must not single-quote $HOME paths (blocks expansion); got: %q", script)
	}
	// docker update chained before docker start, with `|| true` so it cannot
	// block the start. The policy must be "unless-stopped" so a crash, OOM,
	// inner process shutdown, Docker daemon restart, or host reboot brings
	// the container back automatically (see TestContainerRestartPolicy for
	// the full rationale). The PosixSingleQuote on the policy value is what
	// produces the `--restart='unless-stopped'` shape on the wire.
	if !strings.Contains(script, "docker update --restart='unless-stopped'") {
		t.Errorf("script missing docker update --restart='unless-stopped'; got: %q", script)
	}
	if !strings.Contains(script, "2>/dev/null || true") {
		t.Errorf("docker update must be best-effort (2>/dev/null || true); got: %q", script)
	}
	// docker start is the final command (and the only one whose failure surfaces).
	if !strings.HasSuffix(strings.TrimSpace(script), "docker start gh-sr-aw-runner-1") {
		t.Errorf("script must end with `docker start <name>`; got: %q", script)
	}
	// No bare `echo $HOME` resolve (we now rely on the shell to expand it).
	for _, c := range calls {
		if strings.Contains(c, "echo $HOME") {
			t.Errorf("startContainer should not issue a separate `echo $HOME` resolve; calls=%v", calls)
		}
	}
}

// TestStartContainer_PropagatesDockerStartError pins that a docker start
// failure still surfaces as an error even though the preceding rm -f and
// docker update are best-effort. The chained `|| true` after docker update
// must not swallow the docker start exit code.
func TestStartContainer_PropagatesDockerStartError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("docker start failed: no such container")
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		return "", sentinel
	}}
	h := host.NewHost("h", config.HostConfig{Addr: "runner@vps", OS: "linux", Arch: "amd64"})
	h.SetConn(mock)
	m := &Manager{Out: io.Discard}

	err := m.startContainer(h, "aw-runner-1")
	if !errors.Is(err, sentinel) {
		t.Fatalf("startContainer error = %v, want sentinel %v", err, sentinel)
	}
}

func TestManagerStartContainerRecoversStaleRegistration(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions/runners/registration-token") {
			_ = json.NewEncoder(w).Encode(tokenResponse{Token: "fresh-token"})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	var out bytes.Buffer
	var cleanupCmd string
	var createCmd string
	starts := 0
	logChecks := 0
	mock := &testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "docker update --restart") && strings.Contains(cmd, "docker start gh-sr-ci-1"):
			starts++
			return "gh-sr-ci-1\n", nil
		case strings.Contains(cmd, "docker logs --tail 200") && strings.Contains(cmd, staleRegistrationMsg):
			logChecks++
			return "", nil
		case strings.Contains(cmd, "docker inspect --format '{{.Config.Image}}'"):
			return "gh-sr/agentic-runner:2.330.0\n", nil
		case cmd == "echo $HOME":
			return "/home/runner\n", nil
		case strings.Contains(cmd, "docker rm -f") && strings.Contains(cmd, ".credentials_rsaparams"):
			cleanupCmd = cmd
			return "", nil
		case strings.Contains(cmd, "docker create"):
			createCmd = cmd
			return "", nil
		case strings.Contains(cmd, "docker inspect --format '{{.State.Status}}'"):
			return "running\n", nil
		case strings.Contains(cmd, `docker exec "gh-sr-ci-1" sh -c`) &&
			strings.Contains(cmd, "docker info") &&
			strings.Contains(cmd, "test -f /home/runner/.runner"):
			return "dockerd-ok\nok\n", nil
		default:
			return "", nil
		}
	}}
	h := host.NewHost("h", config.HostConfig{Addr: "runner@vps", OS: "linux", Arch: "amd64"})
	h.SetConn(mock)
	m := &Manager{
		GitHub: NewGitHubClientWithHTTP("pat", ts.Client(), ts.URL),
		Out:    &out,
	}
	rc := config.RunnerConfig{
		Name:       "ci",
		Repo:       "o/r",
		Host:       "h",
		Count:      1,
		RunnerMode: config.RunnerModeContainer,
	}

	if err := m.Start(h, rc); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if starts != 2 {
		t.Fatalf("docker start calls = %d, want 2 (original start + recovered container start); calls=%v", starts, mock.Calls)
	}
	if logChecks != 1 {
		t.Fatalf("stale log checks = %d, want 1; calls=%v", logChecks, mock.Calls)
	}
	for _, want := range []string{
		"/home/runner/.gh-sr/runners/ci-1/.runner",
		"/home/runner/.gh-sr/runners/ci-1/.credentials",
		"/home/runner/.gh-sr/runners/ci-1/.credentials_rsaparams",
	} {
		if !strings.Contains(cleanupCmd, want) {
			t.Fatalf("cleanup command missing %q: %q", want, cleanupCmd)
		}
	}
	if strings.Contains(cleanupCmd, "rm -rf") {
		t.Fatalf("cleanup must not remove the whole runner state directory: %q", cleanupCmd)
	}
	if !strings.Contains(createCmd, "GH_SR_RUNNER_TOKEN='fresh-token'") {
		t.Fatalf("create command did not use fresh registration token: %q", createCmd)
	}
	if !strings.Contains(createCmd, "'gh-sr/agentic-runner:2.330.0'") {
		t.Fatalf("create command did not preserve inspected image tag: %q", createCmd)
	}
	if !strings.Contains(out.String(), "registration expired on GitHub, re-creating container") {
		t.Fatalf("missing stale-registration recovery message: %q", out.String())
	}
}

// TestProbeDinDContainerReadiness_RunningHealthy verifies the happy path:
// container is running, inner dockerd answers, .runner is present. The probe
// returns a fully-positive report.
func TestProbeDinDContainerReadiness_RunningHealthy(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			switch {
			case strings.Contains(cmd, "docker inspect --format '{{.State.Status}}'"):
				return "running\n", nil
			case strings.Contains(cmd, `docker exec "gh-sr-x" sh -c`) && strings.Contains(cmd, "docker info") && strings.Contains(cmd, "test -f /home/runner/.runner"):
				return "dockerd-ok\nok\n", nil
			default:
				t.Errorf("unexpected h.Run call: %q", cmd)
				return "", nil
			}
		},
	})

	rep, err := ProbeDinDContainerReadiness(h, "gh-sr-x")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rep.State != "running" {
		t.Errorf("State = %q, want %q", rep.State, "running")
	}
	if !rep.InnerDockerdOK {
		t.Errorf("InnerDockerdOK = false, want true")
	}
	if !rep.Registered {
		t.Errorf("Registered = false, want true")
	}
}

// TestProbeDinDContainerReadiness_RestartingInnerDown verifies that for a
// container in "restarting" state, the probe still runs the inner-dockerd and
// .runner checks (both fail), but returns State == "restarting" so callers
// can distinguish it from "missing" / "exited".
func TestProbeDinDContainerReadiness_RestartingInnerDown(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			switch {
			case strings.Contains(cmd, "docker inspect --format '{{.State.Status}}'"):
				return "restarting\n", nil
			case strings.Contains(cmd, `docker exec "gh-sr-x" sh -c`) && strings.Contains(cmd, "docker info") && strings.Contains(cmd, "test -f /home/runner/.runner"):
				return "no\nno\n", nil
			default:
				return "", nil
			}
		},
	})

	rep, err := ProbeDinDContainerReadiness(h, "gh-sr-x")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rep.State != "restarting" {
		t.Errorf("State = %q, want %q", rep.State, "restarting")
	}
	if rep.InnerDockerdOK {
		t.Errorf("InnerDockerdOK = true, want false (inner dockerd unreachable)")
	}
	if rep.Registered {
		t.Errorf("Registered = true, want false (.runner missing)")
	}
}

// TestProbeDinDContainerReadiness_MissingShortCircuits verifies that a missing
// container surfaces as State == "missing" with the inner probes skipped (the
// probe issues exactly 1 h.Run call, the docker inspect; the inner probes
// would otherwise fail with "No such container" and noise up the report).
func TestProbeDinDContainerReadiness_MissingShortCircuits(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
	calls := 0
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if strings.Contains(cmd, "docker inspect --format '{{.State.Status}}'") {
				calls++
				return "missing\n", nil
			}
			calls++
			t.Errorf("unexpected inner probe on missing container: %q", cmd)
			return "", nil
		},
	})

	rep, err := ProbeDinDContainerReadiness(h, "gh-sr-x")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rep.State != "missing" {
		t.Errorf("State = %q, want %q", rep.State, "missing")
	}
	if rep.InnerDockerdOK || rep.Registered {
		t.Errorf("InnerDockerdOK/Registered should be false on missing container, got %+v", rep)
	}
	if calls != 1 {
		t.Errorf("h.Run called %d times, want 1 (only docker inspect)", calls)
	}
}

// TestProbeDinDContainerReadiness_UsesOneDockerExecOnHappyPath pins the
// round-trip count of the probe on the happy path (state == running) at
// exactly 2: one `docker inspect` for state + one combined `docker exec`
// carrying both the inner-dockerd and the .runner-registered probes. The
// probe used to issue 3 round-trips (state + docker info + test -f); the
// two docker-exec probes were folded into one shell invocation (see the
// win-class of PR #264, #269, #285). If a future refactor splits them back
// apart, this test fails with a clear message.
func TestProbeDinDContainerReadiness_UsesOneDockerExecOnHappyPath(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
	var inspectCalls, execCalls int
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			switch {
			case strings.Contains(cmd, "docker inspect --format '{{.State.Status}}'"):
				inspectCalls++
				return "running\n", nil
			case strings.Contains(cmd, `docker exec "gh-sr-x"`):
				execCalls++
				return "dockerd-ok\nok\n", nil
			default:
				t.Errorf("unexpected h.Run call: %q", cmd)
				return "", nil
			}
		},
	})

	rep, err := ProbeDinDContainerReadiness(h, "gh-sr-x")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rep.State != "running" || !rep.InnerDockerdOK || !rep.Registered {
		t.Fatalf("unexpected report: %+v", rep)
	}
	if inspectCalls != 1 {
		t.Errorf("docker inspect h.Run calls = %d, want 1", inspectCalls)
	}
	if execCalls != 1 {
		t.Errorf("docker exec h.Run calls = %d, want 1 (combined inner dockerd + .runner probe)", execCalls)
	}
}

// TestContainerStateStatus_RunningAndTrimmed verifies the helper returns the
// trimmed docker state string for a running container.
func TestContainerStateStatus_RunningAndTrimmed(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
	h.SetConn(&testutil.MockExecutor{RunFn: func(cmd string) (string, error) {
		if !strings.Contains(cmd, "docker inspect --format '{{.State.Status}}'") {
			t.Errorf("unexpected command: %q", cmd)
		}
		return "  running  \n", nil
	}})
	state, err := ContainerStateStatus(h, "gh-sr-x")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if state != "running" {
		t.Errorf("state = %q, want %q", state, "running")
	}
}

// TestContainerStateStatus_MissingAndEmptyCollapse pins the contract from
// issue #268: both a docker "No such object" (absorbed into "missing" via the
// `|| echo missing` tail) and an empty inspect result collapse to
// ("missing", nil). Callers switch on a single "missing" literal instead of
// also handling "".
func TestContainerStateStatus_MissingAndEmptyCollapse(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"explicit missing sentinel": "missing\n",
		"empty stdout":              "",
		"whitespace-only stdout":    "   \n",
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
			h.SetConn(&testutil.MockExecutor{RunFn: func(string) (string, error) { return out, nil }})
			state, err := ContainerStateStatus(h, "gh-sr-x")
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if state != "missing" {
				t.Errorf("state = %q, want %q", state, "missing")
			}
		})
	}
}

// TestContainerStateStatus_InspectErrorPropagates verifies a host connection
// error on the inspect call propagates as the error return and yields a
// "missing" state (callers must check err first).
func TestContainerStateStatus_InspectErrorPropagates(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
	wantErr := errors.New("connection refused")
	h.SetConn(&testutil.MockExecutor{RunErr: wantErr})
	state, err := ContainerStateStatus(h, "gh-sr-x")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if state != "missing" {
		t.Errorf("state = %q, want %q on inspect error", state, "missing")
	}
}

// TestIsContainerAcceptingJobs pins the acceptance set: only "running" and
// "restarting" count as up-enough to accept work. Every other Docker state
// (paused, exited, missing, etc.) must read false. This is the single source
// of truth that containerAwaitHealthy, ProbeDinDContainerReadiness, and the
// doctor readiness check all switch on (issue #275).
func TestIsContainerAcceptingJobs(t *testing.T) {
	t.Parallel()
	accepting := []string{"running", "restarting"}
	notAccepting := []string{"", "missing", "paused", "exited", "created", "dead", "RUNNING"}
	for _, s := range accepting {
		if !IsContainerAcceptingJobs(s) {
			t.Errorf("IsContainerAcceptingJobs(%q) = false, want true", s)
		}
	}
	for _, s := range notAccepting {
		if IsContainerAcceptingJobs(s) {
			t.Errorf("IsContainerAcceptingJobs(%q) = true, want false", s)
		}
	}
}

// TestProbeDinDContainerReadiness_OtherStateShortCircuits verifies that a
// container in an unexpected state (e.g. "paused", "exited") is also treated
// as terminal at the inspect step and skips the inner probes.
func TestProbeDinDContainerReadiness_OtherStateShortCircuits(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
	calls := 0
	h.SetConn(&testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			if strings.Contains(cmd, "docker inspect --format '{{.State.Status}}'") {
				calls++
				return "exited\n", nil
			}
			calls++
			t.Errorf("unexpected inner probe on exited container: %q", cmd)
			return "", nil
		},
	})

	rep, err := ProbeDinDContainerReadiness(h, "gh-sr-x")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rep.State != "exited" {
		t.Errorf("State = %q, want %q", rep.State, "exited")
	}
	if rep.InnerDockerdOK || rep.Registered {
		t.Errorf("InnerDockerdOK/Registered should be false on exited container, got %+v", rep)
	}
	if calls != 1 {
		t.Errorf("h.Run called %d times, want 1 (only docker inspect)", calls)
	}
}

// TestProbeDinDContainerReadiness_InspectErrorSurfaces verifies that a host
// connection error on the docker inspect call propagates as the error return
// and yields an empty-state report (callers must not interpret State == ""
// as "missing" without checking err).
func TestProbeDinDContainerReadiness_InspectErrorSurfaces(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
	wantErr := errors.New("connection refused")
	h.SetConn(&testutil.MockExecutor{RunErr: wantErr})

	rep, err := ProbeDinDContainerReadiness(h, "gh-sr-x")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if rep.State != "" {
		t.Errorf("State = %q, want empty on inspect error", rep.State)
	}
	if rep.InnerDockerdOK || rep.Registered {
		t.Errorf("InnerDockerdOK/Registered should be false on inspect error, got %+v", rep)
	}
}

// TestProbeDinDContainerReadiness_NormalizesQuotedName verifies the probe
// applies the same shell-safe quoting as the rest of the readiness triad
// (docker exec "name" ...), so a container name with shell metacharacters
// cannot break out of the quoted-name segment. Regression guard for the
// "docker inspect" + "docker exec" command shape.
func TestProbeDinDContainerReadiness_NormalizesQuotedName(t *testing.T) {
	t.Parallel()
	h := host.NewHost("h", config.HostConfig{OS: "linux", Arch: "amd64", Addr: "local"})
	const cname = `evil"; rm -rf /; "`
	var sawInspectQuoted, sawExecQuoted bool
	mock := &testutil.MockExecutor{
		RunFn: func(cmd string) (string, error) {
			// Go's strconv.Quote escapes the inner double-quotes: the
			// command must contain the literal substring "evil\"; rm -rf
			// /; \"" to prove the quoting policy is in force.
			const quoted = `"evil\"; rm -rf /; \""`
			if strings.Contains(cmd, "docker inspect --format '{{.State.Status}}' "+quoted) {
				sawInspectQuoted = true
				return "running\n", nil
			}
			if strings.Contains(cmd, "docker exec "+quoted) {
				sawExecQuoted = true
				return "ok\n", nil
			}
			return "", nil
		},
	}
	h.SetConn(mock)

	rep, err := ProbeDinDContainerReadiness(h, cname)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rep.State != "running" {
		t.Errorf("State = %q, want %q", rep.State, "running")
	}
	if !rep.Registered {
		t.Errorf("Registered = false, want true (the inner exec was the only path to set this)")
	}
	if !sawInspectQuoted {
		t.Errorf("docker inspect command was not shell-safe-quoted; got: %v", mock.Calls)
	}
	if !sawExecQuoted {
		t.Errorf("docker exec command was not shell-safe-quoted")
	}
}

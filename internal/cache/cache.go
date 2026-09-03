// Package cache deploys and manages the per-host local GitHub Actions cache
// server (falcondev-oss/github-actions-cache-server) so container runners can
// serve actions/cache traffic from the host instead of GitHub's cache service.
// One gh-sr-cache container serves every runner container on the same host.
package cache

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/hostshell"
)

const (
	// ContainerName is the Docker container hosting the per-host cache server.
	ContainerName = "gh-sr-cache"
	// DefaultImage is the cache server image. Pin a digest via cache.image in
	// runners.yml for reproducible deploys.
	DefaultImage = "ghcr.io/falcondev-oss/github-actions-cache-server:latest"
	// ContainerPort is the port the server listens on inside the container
	// (Nitro default; the image exposes no port-configuration env).
	ContainerPort = 3000
	// DefaultPort is the host-side published port when cache.port is unset.
	// Fixed and uncommon: below the Linux ephemeral range (32768-60999) so
	// transient outbound connections never claim it, and away from frequent
	// dev-service ports like 3000/8080, which hosts often have bound already.
	DefaultPort = 27420
	// DefaultStoragePath is the host-side storage dir (relative to $HOME) when
	// cache.storage_path is unset.
	DefaultStoragePath = "$HOME/.gh-sr/cache"

	// LayoutLabel is the Docker label stamped on the cache container with the
	// current deploy layout revision. Ensure recreates containers stamped with
	// an older value (or none), so deploy-argument changes converge on the
	// next Ensure instead of silently running a stale deployment forever.
	LayoutLabel    = "gh-sr.cache-layout"
	cacheLayoutRev = "v3" // published-port routing (docker0 gateway, firewall-remediable)
	// networkNamePrev is the bridge network a retired experiment attached the
	// cache and runners to; removed best-effort when recreating a stale one.
	networkNamePrev = "gh-sr"

	// In-container paths for the mounted storage volume; the server reads them
	// from its own env (see lib/schemas.ts upstream). We set them explicitly so
	// the volume layout is pinned regardless of server defaults.
	storageFilesystemPath = "/data/cache"
	dbSQLitePath          = "/data/cache-server.db"

	managementKeyFile = ".management-api-key"
)

// Settings is the effective cache configuration for one operation, built by
// ops from the cache: section of runners.yml. The zero value is disabled.
type Settings struct {
	Enabled          bool
	Port             int
	BindAddr         string // host-side published-port bind; "" = auto (docker0 gateway; 0.0.0.0 fallback + warning). Never used in the runner-facing URL.
	StoragePath      string // "" = DefaultStoragePath; may carry a $HOME prefix
	RetentionDays    int    // 0 = server default (90)
	MaxSizeBytes     int64  // 0 = server default (unbounded)
	MaxUsagePercent  int    // 0 = server default (90)
	Image            string // "" = DefaultImage
	ManagementAPIKey string // "" = read/auto-generate <storage>/.management-api-key
	URLOverride      string // escape hatch: runners get this URL verbatim
}

func (s Settings) image() string {
	if s.Image == "" {
		return DefaultImage
	}
	return s.Image
}

func (s Settings) port() int {
	if s.Port == 0 {
		return DefaultPort
	}
	return s.Port
}

// EffectivePort is the exported form of the resolved host-side published port
// (Port, or DefaultPort when unset) for packages reporting remediation hints.
func (s Settings) EffectivePort() int {
	return s.port()
}

// WithTrailingSlash guarantees the single trailing slash the actions runner
// expects on an ACTIONS_RESULTS_URL-style base URL.
func WithTrailingSlash(u string) string {
	return strings.TrimRight(u, "/") + "/"
}

// ResolveGatewayIP returns the docker0 bridge gateway IPv4 on the host — the
// address runner containers use to reach host-published ports. Empty when the
// host has no docker0 interface (rootless/podman setups).
func ResolveGatewayIP(h *host.Host) (string, error) {
	out, err := h.Run("ip -4 -o addr show docker0 2>/dev/null")
	if err != nil {
		return "", err
	}
	return parseGatewayIPOutput(out), nil
}

// parseGatewayIPOutput extracts the IPv4 address from `ip -4 -o addr show
// docker0` output: "2: docker0    inet 172.17.0.1/16 brd ... " → "172.17.0.1".
func parseGatewayIPOutput(out string) string {
	// SplitSeq avoids the upfront []string allocation that strings.Split makes.
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if f != "inet" || i+1 >= len(fields) {
				continue
			}
			ip := fields[i+1]
			if idx := strings.Index(ip, "/"); idx >= 0 {
				ip = ip[:idx]
			}
			return ip
		}
	}
	return ""
}

// RunnerURL returns the cache base URL to inject into runners on this host
// (with trailing slash), or "" when injection must be skipped.
//
// Precedence: URLOverride → explicit non-0.0.0.0 BindAddr → docker0 gateway.
// A gateway lookup failure yields "" (deploy then binds 0.0.0.0 and the
// caller warns), because without a bridge gateway there is no
// container-reachable address to inject.
//
// Note this is a host-published port: reaching it from a container traverses
// the host's INPUT chain, so hosts with a default-deny firewall (ufw et al)
// need one allow rule — `gh sr doctor` probes the exact URL from inside a
// runner and prints the command when it is blocked.
func (s Settings) RunnerURL(h *host.Host) string {
	if s.URLOverride != "" {
		return WithTrailingSlash(s.URLOverride)
	}
	if s.BindAddr != "" && s.BindAddr != "0.0.0.0" {
		return fmt.Sprintf("http://%s:%d/", s.BindAddr, s.port())
	}
	gw, err := ResolveGatewayIP(h)
	if err != nil || gw == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d/", gw, s.port())
}

// localURL is the cache API base as seen from the host itself: an explicit
// bind address wins; 0.0.0.0 / auto bind answers on the docker0 gateway (a
// host-local address), falling back to loopback when no gateway was found.
func (s Settings) localURL(gatewayIP string) string {
	hostPort := s.BindAddr
	if hostPort == "" || hostPort == "0.0.0.0" {
		if gatewayIP != "" {
			hostPort = gatewayIP
		} else {
			hostPort = "127.0.0.1"
		}
	}
	return fmt.Sprintf("http://%s:%d", hostPort, s.port())
}

// resolvedStoragePath expands the $HOME prefix (the default storage path is
// expressed relative to the remote shell's home) into an absolute path —
// Docker bind mounts and mkdir do not undergo shell expansion.
func (s Settings) resolvedStoragePath(h *host.Host) (string, error) {
	p := s.StoragePath
	if p == "" {
		p = DefaultStoragePath
	}
	if !strings.HasPrefix(p, "$HOME") {
		return p, nil
	}
	out, err := h.Run("echo $HOME")
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	home := strings.TrimSpace(out)
	if home == "" {
		return "", fmt.Errorf("resolving home dir: empty $HOME")
	}
	return home + p[len("$HOME"):], nil
}

// containerState reports "running", "exists" (present but not running) or ""
// (absent) for the cache container.
func containerState(h *host.Host) (string, error) {
	out, err := h.Run(fmt.Sprintf(
		"docker inspect --format='{{.State.Status}}' %s 2>/dev/null || true", ContainerName))
	if err != nil {
		return "", fmt.Errorf("inspecting cache container: %w", err)
	}
	state := strings.TrimSpace(out)
	switch state {
	case "running":
		return "running", nil
	case "":
		return "", nil
	default:
		return "exists", nil
	}
}

// cacheLayoutCurrent reports whether an existing cache container carries the
// current deploy layout label. Used by Ensure to converge deployments made by
// older gh-sr versions (or after a layout change): a stale container cannot
// be updated in place (its env and ports are baked at create), so it is
// recreated.
func cacheLayoutCurrent(h *host.Host) (bool, error) {
	out, err := h.Run(fmt.Sprintf(
		"docker inspect --format '{{.State.Status}}|{{index .Config.Labels %q}}' %s 2>/dev/null || true",
		LayoutLabel, ContainerName))
	if err != nil {
		return false, fmt.Errorf("inspecting cache container layout: %w", err)
	}
	return strings.Contains(out, "|"+cacheLayoutRev), nil
}

// Ensure deploys the cache server idempotently: a running container with the
// current layout label is a no-op, a stopped one is started, a missing one is
// pulled and created, and one stamped with an older layout (e.g. deployed by
// an earlier gh-sr) is recreated. The management API key comes from Settings
// or (generated on first deploy, kept for later prune/status calls)
// <storage>/.management-api-key.
func Ensure(w io.Writer, h *host.Host, s Settings) error {
	if !s.Enabled {
		return nil
	}
	state, err := containerState(h)
	if err != nil {
		return err
	}
	if state != "" {
		current, err := cacheLayoutCurrent(h)
		if err != nil {
			return err
		}
		if !current {
			fmt.Fprintf(w, "  cache: %s was deployed by an older gh-sr layout; recreating...\n", ContainerName)
			if _, err := h.Run("docker rm -f " + ContainerName); err != nil {
				return fmt.Errorf("removing outdated cache container: %w", err)
			}
			// Best-effort cleanup of the bridge network a retired deploy
			// layout attached the cache and runners to.
			_, _ = h.Run("docker network rm " + networkNamePrev + " 2>/dev/null || true")
			state = ""
		}
	}
	switch state {
	case "running":
		return nil
	case "exists":
		fmt.Fprintf(w, "  cache: starting %s...\n", ContainerName)
		if _, err := h.Run("docker start " + ContainerName); err != nil {
			return fmt.Errorf("starting cache container: %w", err)
		}
		return nil
	default:
		return deploy(w, h, s)
	}
}

func deploy(w io.Writer, h *host.Host, s Settings) error {
	storage, err := s.resolvedStoragePath(h)
	if err != nil {
		return err
	}
	if _, err := h.Run("mkdir -p " + hostshell.PosixSingleQuote(storage)); err != nil {
		return fmt.Errorf("creating cache storage dir: %w", err)
	}

	key := s.ManagementAPIKey
	if key == "" {
		key, err = ensureManagementKey(h, storage)
		if err != nil {
			return err
		}
	}

	fmt.Fprintf(w, "  cache: pulling %s...\n", s.image())
	// Pull is best-effort: a pre-pulled image must still deploy on hosts
	// without registry access. A genuinely missing image fails at docker run.
	_, _ = h.Run("docker pull " + hostshell.PosixSingleQuote(s.image()))

	bind := s.BindAddr
	if bind == "" {
		gw, err := ResolveGatewayIP(h)
		if err != nil || gw == "" {
			bind = "0.0.0.0"
			fmt.Fprintf(w, "  cache: warning: docker0 gateway not found; binding %s (cache API is exposed on every host interface)\n", bind)
		} else {
			bind = gw
		}
	}

	envs := []string{
		// Runner-facing base URL: the server signs cache download URLs with
		// it, so it must be an address the runner containers can reach — the
		// same published host port the runners are injected with.
		"API_BASE_URL=" + s.RunnerURL(h),
		"STORAGE_DRIVER=filesystem",
		"STORAGE_FILESYSTEM_PATH=" + storageFilesystemPath,
		"DB_DRIVER=sqlite",
		"DB_SQLITE_PATH=" + dbSQLitePath,
	}
	if s.RetentionDays > 0 {
		envs = append(envs, fmt.Sprintf("CACHE_CLEANUP_OLDER_THAN_DAYS=%d", s.RetentionDays))
	}
	if s.MaxSizeBytes > 0 {
		envs = append(envs, fmt.Sprintf("CACHE_MAX_SIZE_BYTES=%d", s.MaxSizeBytes))
	}
	if s.MaxUsagePercent > 0 {
		envs = append(envs, fmt.Sprintf("CACHE_FILESYSTEM_MAX_USAGE_PERCENT=%d", s.MaxUsagePercent))
	}
	if key != "" {
		envs = append(envs, "MANAGEMENT_API_KEY="+key)
	}

	args := []string{
		"docker", "run", "-d",
		"--name", ContainerName,
		"--restart", "unless-stopped",
		"--label", LayoutLabel + "=" + cacheLayoutRev,
		"-p", fmt.Sprintf("%s:%d:%d", bind, s.port(), ContainerPort),
		"-v", fmt.Sprintf("%s:%s", hostshell.PosixSingleQuote(storage), "/data"),
	}
	for _, e := range envs {
		args = append(args, "-e", hostshell.PosixSingleQuote(e))
	}
	args = append(args, hostshell.PosixSingleQuote(s.image()))

	fmt.Fprintf(w, "  cache: starting %s on %s:%d (storage %s)...\n", ContainerName, bind, s.port(), storage)
	if _, err := h.Run(strings.Join(args, " ")); err != nil {
		return fmt.Errorf("starting cache container: %w", err)
	}
	return nil
}

// ensureManagementKey returns the persisted management API key, generating and
// storing one (0600) when the storage dir has none. Only called at deploy
// time, when the old container (which baked the previous key into its env) is
// gone — overwriting a stale file then is safe.
func ensureManagementKey(h *host.Host, storage string) (string, error) {
	path := storage + "/" + managementKeyFile
	q := hostshell.PosixSingleQuote
	out, err := h.Run(fmt.Sprintf("test -f %s && cat %s", q(path), q(path)))
	if err == nil {
		if key := strings.TrimSpace(out); key != "" {
			return key, nil
		}
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating management API key: %w", err)
	}
	key := hex.EncodeToString(buf)
	if _, err := h.Run(fmt.Sprintf("umask 077; printf '%%s\\n' %s > %s", q(key), q(path))); err != nil {
		return "", fmt.Errorf("storing management API key: %w", err)
	}
	return key, nil
}

// managementKey resolves the key for management-API calls: Settings first,
// then the generated file. Empty when neither exists (the API is disabled
// server-side in that case and needs no prune).
func (s Settings) managementKey(h *host.Host, storage string) string {
	if s.ManagementAPIKey != "" {
		return s.ManagementAPIKey
	}
	path := storage + "/" + managementKeyFile
	out, err := h.Run(fmt.Sprintf("test -f %s && cat %s",
		hostshell.PosixSingleQuote(path), hostshell.PosixSingleQuote(path)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Inspect gathers the cache server state for `gh sr cache status`. The health
// probe is best-effort: failures set Healthy=false instead of erroring.
func Inspect(h *host.Host, s Settings) (StatusInfo, error) {
	info := StatusInfo{}
	out, err := h.Run(fmt.Sprintf(
		"docker inspect --format='{{.State.Status}}|{{.Config.Image}}' %s 2>/dev/null || true", ContainerName))
	if err != nil {
		return info, fmt.Errorf("inspecting cache container: %w", err)
	}
	if parts := strings.SplitN(strings.TrimSpace(out), "|", 2); parts[0] != "" {
		info.State = parts[0]
		if len(parts) == 2 {
			info.Image = parts[1]
		}
	}

	storage, err := s.resolvedStoragePath(h)
	if err != nil {
		return info, err
	}
	info.StoragePath = storage
	info.URL = s.RunnerURL(h)

	if info.State != "" {
		gw, _ := ResolveGatewayIP(h)
		healthOut, healthErr := h.Run(fmt.Sprintf(
			"curl -fsS -m 3 %s/health 2>/dev/null || true", hostshell.PosixSingleQuote(s.localURL(gw))))
		info.Healthy = healthErr == nil && strings.TrimSpace(healthOut) == "healthy"

		if duOut, duErr := h.Run(fmt.Sprintf(
			"du -sb %s 2>/dev/null | cut -f1", hostshell.PosixSingleQuote(storage))); duErr == nil {
			if n, perr := strconv.ParseInt(strings.TrimSpace(duOut), 10, 64); perr == nil {
				info.StorageBytes = n
			}
		}
	}
	return info, nil
}

// MeasureStorage reports the resolved on-host storage path and its size in
// bytes (0 when the dir does not exist yet or is empty).
func MeasureStorage(h *host.Host, s Settings) (string, int64, error) {
	storage, err := s.resolvedStoragePath(h)
	if err != nil {
		return "", 0, err
	}
	out, err := h.Run(fmt.Sprintf("du -sb %s 2>/dev/null | cut -f1", hostshell.PosixSingleQuote(storage)))
	if err != nil {
		return storage, 0, err
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if perr != nil {
		return storage, 0, nil
	}
	return storage, n, nil
}

// StatusInfo is the reported state of the per-host cache server.
type StatusInfo struct {
	State        string // Docker state ("running", "exited", ...) or "" when absent
	Image        string
	URL          string // runner-facing base URL ("" when not resolvable)
	StoragePath  string
	StorageBytes int64
	Healthy      bool // GET /health answered "healthy"
}

// Prune deletes every cache entry via the management API; the server then
// reclaims the orphaned storage. Best-effort by design: when the key is
// missing or the API is unreachable/disabled, a hint is printed and the
// operation succeeds — the management surface is version-dependent.
func Prune(w io.Writer, h *host.Host, s Settings) error {
	storage, err := s.resolvedStoragePath(h)
	if err != nil {
		return err
	}
	key := s.managementKey(h, storage)
	if key == "" {
		fmt.Fprintf(w, "  cache: no management API key configured (cache.management_api_key or %s/%s); the server has its management API disabled, nothing to prune\n",
			storage, managementKeyFile)
		return nil
	}

	gw, _ := ResolveGatewayIP(h)
	base := s.localURL(gw)
	fmt.Fprintf(w, "  cache: pruning all cache entries via %s...\n", base)
	cmd := fmt.Sprintf("curl -fsS -m 60 -X DELETE -H 'X-Api-Key: %s' %s/management-api/cache-entries/",
		key, hostshell.PosixSingleQuote(base))
	if _, err := h.Run(cmd); err != nil {
		fmt.Fprintf(w, "  cache: warning: prune request failed (%v); the management API may be disabled or this server version may not expose it — cache entries also expire on their own (retention/max-size cleanup)\n", err)
		return nil
	}
	fmt.Fprintf(w, "  cache: all cache entries deleted; storage is reclaimed by the next cleanup pass\n")
	return nil
}

// Remove stops and deletes the cache container. purgeData also deletes the
// storage dir (every cached byte plus the generated management key). This is
// the only uninstall path — `gh sr remove` never touches the cache.
func Remove(w io.Writer, h *host.Host, s Settings, purgeData bool) error {
	if _, err := h.Run("docker rm -f " + ContainerName); err != nil {
		return fmt.Errorf("removing cache container: %w", err)
	}
	fmt.Fprintf(w, "  cache: removed %s\n", ContainerName)
	if !purgeData {
		return nil
	}
	storage, err := s.resolvedStoragePath(h)
	if err != nil {
		return err
	}
	if _, err := h.Run("rm -rf " + hostshell.PosixSingleQuote(storage)); err != nil {
		return fmt.Errorf("removing cache storage: %w", err)
	}
	fmt.Fprintf(w, "  cache: purged %s\n", storage)
	return nil
}

package ops

import (
	"fmt"
	"io"
	"sort"

	"github.com/an-lee/gh-sr/internal/cache"
	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/runner"
)

// cacheSettings builds the effective cache settings from runners.yml. nil when
// the cache is disabled (zero-value Configs without a Load are disabled — the
// enabled default is applied by config.applyDefaults).
func cacheSettings(cfg *config.Config) *cache.Settings {
	return cache.SettingsFromConfig(cfg)
}

// ensureCacheForHost deploys the per-host cache container before container
// runner work. Idempotent and cheap when already running (one docker inspect).
func ensureCacheForHost(w io.Writer, h *host.Host, s *cache.Settings) error {
	if s == nil {
		return nil
	}
	return cache.Ensure(w, h, *s)
}

// cacheTargets returns the sorted host names a cache command should run on:
// Linux hosts only (the cache server is a Linux/Docker workload), optionally
// narrowed by --host.
func cacheTargets(w io.Writer, cfg *config.Config, filterHost string) ([]string, error) {
	if err := ResolveHostInfo(w, cfg); err != nil {
		return nil, err
	}
	var hosts []string
	for name, h := range cfg.Hosts {
		if filterHost != "" && name != filterHost {
			continue
		}
		if h.OS != "linux" {
			fmt.Fprintf(w, "Skipping host %s (os: %s; the cache server requires Linux)\n", name, h.OS)
			continue
		}
		hosts = append(hosts, name)
	}
	sort.Strings(hosts)
	return hosts, nil
}

// CacheDeploy ensures the per-host cache server container exists and runs.
func CacheDeploy(w io.Writer, cfg *config.Config, filterHost string) error {
	s := cacheSettings(cfg)
	if s == nil {
		return fmt.Errorf("cache is disabled in runners.yml (cache.enabled: false); enable it or remove the setting")
	}
	hosts, err := cacheTargets(w, cfg, filterHost)
	if err != nil {
		return err
	}
	for _, name := range hosts {
		writeHostBanner(w, "Deploying cache on "+name, cfg.Hosts[name].Addr)
		h, err := connectHostFn(name, cfg.Hosts[name])
		if err != nil {
			return err
		}
		err = cache.Ensure(w, h, *s)
		h.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// CacheStatus reports the per-host cache server state, health, and storage.
func CacheStatus(w io.Writer, cfg *config.Config, filterHost string) error {
	s := cacheSettings(cfg)
	hosts, err := cacheTargets(w, cfg, filterHost)
	if err != nil {
		return err
	}
	for _, name := range hosts {
		writeHostBanner(w, "Cache status on "+name, cfg.Hosts[name].Addr)
		h, err := connectHostFn(name, cfg.Hosts[name])
		if err != nil {
			return err
		}
		if s == nil {
			fmt.Fprintf(w, "  cache: disabled in runners.yml\n")
			h.Close()
			continue
		}
		info, err := cache.Inspect(h, *s)
		h.Close()
		if err != nil {
			return err
		}
		printCacheStatus(w, info)
	}
	return nil
}

func printCacheStatus(w io.Writer, info cache.StatusInfo) {
	if info.State == "" {
		fmt.Fprintf(w, "  cache: not deployed\n")
		return
	}
	state := info.State
	if info.Healthy {
		state += " (healthy)"
	}
	fmt.Fprintf(w, "  state:   %s\n", state)
	fmt.Fprintf(w, "  image:   %s\n", info.Image)
	if info.URL != "" {
		fmt.Fprintf(w, "  url:     %s\n", info.URL)
	}
	fmt.Fprintf(w, "  storage: %s (%s)\n", info.StoragePath, runner.FormatBytesHuman(info.StorageBytes))
}

// CachePrune deletes all cache entries on every target host (best-effort per host).
func CachePrune(w io.Writer, cfg *config.Config, filterHost string) error {
	s := cacheSettings(cfg)
	hosts, err := cacheTargets(w, cfg, filterHost)
	if err != nil {
		return err
	}
	for _, name := range hosts {
		writeHostBanner(w, "Pruning cache on "+name, cfg.Hosts[name].Addr)
		h, err := connectHostFn(name, cfg.Hosts[name])
		if err != nil {
			return err
		}
		if s == nil {
			fmt.Fprintf(w, "  cache: disabled in runners.yml\n")
			h.Close()
			continue
		}
		err = cache.Prune(w, h, *s)
		h.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// CacheRemove uninstalls the cache server; purgeData also deletes stored cache
// data. Runner removal (gh sr remove) never touches the cache — this command
// is the only uninstall path.
func CacheRemove(w io.Writer, cfg *config.Config, filterHost string, purgeData bool) error {
	s := cacheSettings(cfg)
	hosts, err := cacheTargets(w, cfg, filterHost)
	if err != nil {
		return err
	}
	for _, name := range hosts {
		writeHostBanner(w, "Removing cache on "+name, cfg.Hosts[name].Addr)
		h, err := connectHostFn(name, cfg.Hosts[name])
		if err != nil {
			return err
		}
		var settings cache.Settings
		if s != nil {
			settings = *s
		}
		err = cache.Remove(w, h, settings, purgeData)
		h.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

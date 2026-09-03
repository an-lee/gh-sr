package cache

import "github.com/an-lee/gh-sr/internal/config"

// SettingsFromConfig builds Settings from the cache: section of runners.yml.
// nil when the cache is disabled — zero-value configs (no Load) are disabled;
// the enabled-by-default default is applied by config.applyDefaults.
func SettingsFromConfig(cfg *config.Config) *Settings {
	if cfg == nil || !cfg.CacheEnabled() {
		return nil
	}
	return &Settings{
		Enabled:          true,
		Port:             cfg.Cache.Port,
		BindAddr:         cfg.Cache.BindAddr,
		StoragePath:      cfg.Cache.StoragePath,
		RetentionDays:    cfg.Cache.RetentionDays,
		MaxSizeBytes:     cfg.Cache.MaxSizeBytes,
		MaxUsagePercent:  cfg.Cache.MaxUsagePercent,
		Image:            cfg.Cache.Image,
		ManagementAPIKey: cfg.Cache.ManagementAPIKey,
		URLOverride:      cfg.Cache.URLOverride,
	}
}

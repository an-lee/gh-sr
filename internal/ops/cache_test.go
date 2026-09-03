package ops

import (
	"testing"

	"github.com/an-lee/gh-sr/internal/config"
)

func TestCacheSettings_mapping(t *testing.T) {
	t.Parallel()
	if got := cacheSettings(nil); got != nil {
		t.Errorf("nil cfg: got %+v", got)
	}
	if got := cacheSettings(&config.Config{}); got != nil {
		t.Errorf("zero-value cfg (cache disabled): got %+v", got)
	}

	enabled := true
	cfg := &config.Config{Cache: config.CacheConfig{
		Enabled:          &enabled,
		Port:             3001,
		BindAddr:         "172.17.0.1",
		StoragePath:      "/srv/cache",
		RetentionDays:    30,
		MaxSizeBytes:     1024,
		MaxUsagePercent:  80,
		Image:            "example.com/cache:v1",
		ManagementAPIKey: "k",
		URLOverride:      "http://10.0.0.5:3000/",
	}}
	got := cacheSettings(cfg)
	if got == nil || !got.Enabled {
		t.Fatalf("enabled cfg: got %+v", got)
	}
	want := config.CacheConfig(cfg.Cache)
	if got.Port != want.Port || got.BindAddr != want.BindAddr ||
		got.StoragePath != want.StoragePath || got.RetentionDays != want.RetentionDays ||
		got.MaxSizeBytes != want.MaxSizeBytes || got.MaxUsagePercent != want.MaxUsagePercent ||
		got.Image != want.Image || got.ManagementAPIKey != want.ManagementAPIKey ||
		got.URLOverride != want.URLOverride {
		t.Errorf("settings mismatch: got %+v, want %+v", got, cfg.Cache)
	}
}

func TestEnsureCacheForHost_nilSettingsNoop(t *testing.T) {
	t.Parallel()
	if err := ensureCacheForHost(nil, nil, nil); err != nil {
		t.Errorf("nil settings must be a no-op: %v", err)
	}
}

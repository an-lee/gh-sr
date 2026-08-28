package config

import (
	"fmt"
	"sync"

	ghAuth "github.com/cli/go-gh/v2/pkg/auth"
)

// ResolveToken returns a GitHub API token from the gh CLI (go-gh reads gh's config/keyring).
// Config must be validated first: legacy github.pat is rejected in Config.Validate.
func ResolveToken(_ *Config) (string, error) {
	token, err := ghAuth.TokenForHost("github.com")
	if token != "" {
		return token, nil
	}
	return "", fmt.Errorf("no GitHub token found: %v; run `gh auth login`", err)
}

// tokenAvailableOnce memoizes the first probe of `gh auth` so callers that
// only care about *whether* a token exists (UI hints like "(from gh CLI)" vs
// "(none)") don't repeat the underlying os/exec.LookPath("gh") + `gh auth
// token` round-trip. The probe is stable for the lifetime of the process:
// users don't typically log in/out mid-TUI-session, and a stale display
// value here is a cosmetic miss, not a correctness issue. The full token is
// NOT cached — callers that need the live token still call ResolveToken.
var (
	tokenAvailableOnce sync.Once
	tokenAvailable     bool
)

// IsTokenAvailable reports whether a GitHub token can be resolved without
// returning the token itself. The first call probes `gh auth` once via
// ghAuth.TokenForHost; subsequent calls return the cached result. Safe for
// concurrent use.
func IsTokenAvailable() bool {
	tokenAvailableOnce.Do(func() {
		token, _ := ghAuth.TokenForHost("github.com")
		tokenAvailable = token != ""
	})
	return tokenAvailable
}

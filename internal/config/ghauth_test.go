package config

import (
	"testing"
)

func TestResolveToken_FromGhOrErrors(t *testing.T) {
	t.Setenv("GITHUB_PAT", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	cfg := &Config{}
	tok, err := ResolveToken(cfg)
	if err != nil {
		// No gh auth configured — expected error path.
		if tok != "" {
			t.Errorf("token should be empty on error, got %q", tok)
		}
		return
	}
	if tok == "" {
		t.Fatal("token from gh should not be empty")
	}
}

// TestIsTokenAvailable_ConsistentWithResolveToken verifies that
// IsTokenAvailable agrees with the (token != "") half of ResolveToken.
// Note: the package-level sync.Once memoizes the first probe, so this
// test does not reset the cache — once the first call has populated it,
// subsequent calls must agree with each other (idempotency check), and
// the bool must agree with whatever ResolveToken would have returned.
func TestIsTokenAvailable_ConsistentWithResolveToken(t *testing.T) {
	t.Setenv("GITHUB_PAT", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	// Warm the cache by calling once; then verify it is idempotent.
	first := IsTokenAvailable()
	for i := 0; i < 3; i++ {
		if got := IsTokenAvailable(); got != first {
			t.Fatalf("IsTokenAvailable not idempotent: first=%v, later=%v", first, got)
		}
	}
	// Cross-check against the underlying ResolveToken contract: a non-empty
	// token must imply IsTokenAvailable() == true. (The reverse isn't
	// guaranteed because TokenForHost may also surface sources that
	// ResolveToken doesn't propagate — but the forward direction is.)
	tok, err := ResolveToken(&Config{})
	if err == nil && tok != "" && !first {
		t.Errorf("ResolveToken returned a token but IsTokenAvailable reported false")
	}
}

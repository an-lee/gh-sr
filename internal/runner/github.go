package runner

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
)

type GitHubClient struct {
	pat     string
	client  *http.Client
	apiBase string // e.g. https://api.github.com; no trailing slash

	// Cached latest runner version, fetched once per client instance.
	latestVersion     string
	latestVersionOnce sync.Once
	latestVersionErr  error

	// getCache memoises successful GET responses by URL together with the
	// server-supplied ETag. The next call to get() for the same URL sends
	// If-None-Match; a 304 Not Modified response short-circuits the body
	// transfer and the downstream json.Unmarshal, both of which would otherwise
	// run on every TUI 5s refresh tick. The cache is keyed on the full URL so
	// pagination (?page=N) and scope/target variations coexist without
	// cross-contamination. Only populated for read endpoints reached through
	// get(); POST (token) and DELETE (runner removal) are deliberately not
	// cached because their response semantics are per-call, not idempotent.
	getCacheMu sync.RWMutex
	getCache   map[string]getCacheEntry
}

// getCacheEntry pairs a GitHub ETag with the body bytes returned for that URL.
// body is treated as read-only after storage; callers receive a defensive copy
// to keep cached bytes immutable across goroutines that race on the same URL.
type getCacheEntry struct {
	etag string
	body []byte
}

type GitHubRunner struct {
	ID     int64   `json:"id"`
	Name   string  `json:"name"`
	OS     string  `json:"os"`
	Status string  `json:"status"`
	Busy   bool    `json:"busy"`
	Labels []Label `json:"labels"`
}

type Label struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type runnersResponse struct {
	TotalCount int            `json:"total_count"`
	Runners    []GitHubRunner `json:"runners"`
}

type tokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

type releaseResponse struct {
	TagName string `json:"tag_name"`
}

func NewGitHubClient(pat string) *GitHubClient {
	return NewGitHubClientWithHTTP(pat, nil, "")
}

// NewGitHubClientWithHTTP builds a client with a custom HTTP client and API base URL (for tests).
func NewGitHubClientWithHTTP(pat string, client *http.Client, apiBase string) *GitHubClient {
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	if client == nil {
		client = &http.Client{}
	}
	return &GitHubClient{
		pat:      pat,
		client:   client,
		apiBase:  strings.TrimRight(apiBase, "/"),
		getCache: make(map[string]getCacheEntry),
	}
}

func (g *GitHubClient) repoActionsURL(repo, rest string) string {
	return fmt.Sprintf("%s/repos/%s/actions/%s", g.apiBase, repo, rest)
}

func (g *GitHubClient) orgActionsURL(org, rest string) string {
	return fmt.Sprintf("%s/orgs/%s/actions/%s", g.apiBase, org, rest)
}

// actionsURL returns the correct GitHub API URL for runners based on scope.
func (g *GitHubClient) actionsURL(scope, target, rest string) string {
	if scope == "org" {
		return g.orgActionsURL(target, rest)
	}
	return g.repoActionsURL(target, rest)
}

// fetchToken POSTs to the given GitHub Actions endpoint and parses the token
// response. opLabel is used in error messages (e.g. "registration" or
// "removal"). emptyHint, if non-empty, is appended in parentheses to the
// empty-token error to give operators actionable guidance.
func (g *GitHubClient) fetchToken(scope, target, endpoint, opLabel, emptyHint string) (string, error) {
	url := g.actionsURL(scope, target, endpoint)
	resp, err := g.post(url, nil)
	if err != nil {
		return "", fmt.Errorf("getting %s token for %s: %w", opLabel, target, err)
	}

	var tok tokenResponse
	if err := json.Unmarshal(resp, &tok); err != nil {
		return "", fmt.Errorf("parsing %s token: %w", opLabel, err)
	}
	if tok.Token == "" {
		if emptyHint != "" {
			return "", fmt.Errorf("empty %s token for %s (%s)", opLabel, target, emptyHint)
		}
		return "", fmt.Errorf("empty %s token for %s", opLabel, target)
	}
	return tok.Token, nil
}

func (g *GitHubClient) GetRegistrationTokenScoped(scope, target string) (string, error) {
	return g.fetchToken(scope, target, "runners/registration-token", "registration", "check GitHub token and repo admin access")
}

func (g *GitHubClient) GetRemovalTokenScoped(scope, target string) (string, error) {
	return g.fetchToken(scope, target, "runners/remove-token", "removal", "")
}

func (g *GitHubClient) ListRunnersScoped(scope, target string) ([]GitHubRunner, error) {
	const perPage = 100
	var all []GitHubRunner
	for page := 1; ; page++ {
		url := fmt.Sprintf("%s?per_page=%d&page=%d", g.actionsURL(scope, target, "runners"), perPage, page)
		resp, err := g.get(url)
		if err != nil {
			return nil, fmt.Errorf("listing runners for %s: %w", target, err)
		}
		var rr runnersResponse
		if err := json.Unmarshal(resp, &rr); err != nil {
			return nil, fmt.Errorf("parsing runners list: %w", err)
		}
		all = append(all, rr.Runners...)
		if len(rr.Runners) < perPage {
			break
		}
	}
	return all, nil
}

func (g *GitHubClient) DeleteRunnerScoped(scope, target string, runnerID int64) error {
	var url string
	if scope == "org" {
		url = fmt.Sprintf("%s/orgs/%s/actions/runners/%d", g.apiBase, target, runnerID)
	} else {
		url = fmt.Sprintf("%s/repos/%s/actions/runners/%d", g.apiBase, target, runnerID)
	}
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return err
	}
	g.setHeaders(req)

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("deleting runner %d: %w", runnerID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete runner %d: HTTP %d: %s", runnerID, resp.StatusCode, string(body))
	}
	return nil
}

// LockWorkflow is one compiled gh-aw workflow file (*.lock.yml) fetched from a
// repository's .github/workflows directory.
type LockWorkflow struct {
	Name    string
	Content string
}

type contentsEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type contentsFile struct {
	Name     string `json:"name"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

// ListLockWorkflows returns up to limit *.lock.yml files from the repo's
// .github/workflows directory (alphabetical order, truncated at limit).
func (g *GitHubClient) ListLockWorkflows(repo string, limit int) ([]LockWorkflow, error) {
	listURL := fmt.Sprintf("%s/repos/%s/contents/.github/workflows", g.apiBase, repo)
	body, err := g.get(listURL)
	if err != nil {
		return nil, fmt.Errorf("listing .github/workflows for %s: %w", repo, err)
	}
	var entries []contentsEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("parsing workflows listing for %s: %w", repo, err)
	}
	var names []string
	for _, e := range entries {
		if e.Type == "file" && strings.HasSuffix(e.Name, ".lock.yml") {
			names = append(names, e.Name)
		}
	}
	sort.Strings(names)
	if len(names) > limit {
		names = names[:limit]
	}
	out := make([]LockWorkflow, 0, len(names))
	for _, name := range names {
		fileURL := fmt.Sprintf("%s/repos/%s/contents/.github/workflows/%s", g.apiBase, repo, url.PathEscape(name))
		fb, err := g.get(fileURL)
		if err != nil {
			return nil, fmt.Errorf("fetching %s in %s: %w", name, repo, err)
		}
		var f contentsFile
		if err := json.Unmarshal(fb, &f); err != nil {
			return nil, fmt.Errorf("parsing %s in %s: %w", name, repo, err)
		}
		content := f.Content
		if f.Encoding == "base64" {
			dec, derr := base64.StdEncoding.DecodeString(strings.ReplaceAll(content, "\n", ""))
			if derr != nil {
				return nil, fmt.Errorf("decoding %s in %s: %w", name, repo, derr)
			}
			content = string(dec)
		}
		out = append(out, LockWorkflow{Name: name, Content: content})
	}
	return out, nil
}

func (g *GitHubClient) GetLatestRunnerVersion() (string, error) {
	g.latestVersionOnce.Do(func() {
		url := fmt.Sprintf("%s/repos/actions/runner/releases/latest", g.apiBase)
		resp, err := g.get(url)
		if err != nil {
			g.latestVersionErr = fmt.Errorf("fetching latest runner version: %w", err)
			return
		}

		var rel releaseResponse
		if err := json.Unmarshal(resp, &rel); err != nil {
			g.latestVersionErr = fmt.Errorf("parsing release: %w", err)
			return
		}

		version := strings.TrimPrefix(rel.TagName, "v")
		if version == "" {
			g.latestVersionErr = fmt.Errorf("empty version from GitHub releases API")
			return
		}
		g.latestVersion = version
	})
	return g.latestVersion, g.latestVersionErr
}

func (g *GitHubClient) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+g.pat)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

func (g *GitHubClient) get(url string) ([]byte, error) {
	g.getCacheMu.RLock()
	cached, ok := g.getCache[url]
	g.getCacheMu.RUnlock()

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	g.setHeaders(req)
	if ok && cached.etag != "" {
		// Conditional GET: GitHub returns 304 Not Modified when the resource
		// is unchanged, letting us skip the body transfer entirely.
		req.Header.Set("If-None-Match", cached.etag)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified && ok {
		// Defensive copy: cached.body is shared across goroutines that race
		// on the same URL during a TUI refresh, and downstream json.Unmarshal
		// keeps the slice alive for the duration of the parse.
		out := make([]byte, len(cached.body))
		copy(out, cached.body)
		return out, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	if etag := resp.Header.Get("ETag"); etag != "" {
		// Store a copy so the cache owns its bytes independent of any future
		// resp.Body reuse by the http.Client transport.
		stored := make([]byte, len(body))
		copy(stored, body)
		g.getCacheMu.Lock()
		g.getCache[url] = getCacheEntry{etag: etag, body: stored}
		g.getCacheMu.Unlock()
	}

	return body, nil
}

func (g *GitHubClient) post(url string, payload io.Reader) ([]byte, error) {
	req, err := http.NewRequest("POST", url, payload)
	if err != nil {
		return nil, err
	}
	g.setHeaders(req)

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

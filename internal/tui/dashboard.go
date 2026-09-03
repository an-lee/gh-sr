package tui

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/doctor"
	"github.com/an-lee/gh-sr/internal/editor"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/ops"
	"github.com/an-lee/gh-sr/internal/runner"
)

// NonTTYHint is printed when stdout is not a terminal and the dashboard cannot run.
const NonTTYHint = `gh sr: stdout is not a terminal; open the dashboard on a TTY, or use "gh sr status" and "gh sr --help".`

// DashboardOpts configures the interactive dashboard (paths for reload and doctor).
type DashboardOpts struct {
	ConfigPath  string
	EnvPath     string
	FilterHost  string
	FilterRepo  string
	GhSrVersion string
}

type panelKind int

const (
	panelMain panelKind = iota
	panelActionMenu
	panelGlobalMenu
	panelFilterMenu
	panelFilterHost
	panelFilterRepo
	panelConfirmCleanup
	panelConfirmRemove
	panelHostMetrics
	panelScroll
)

type dashboardModel struct {
	opts DashboardOpts

	cfg *config.Config
	mgr *runner.Manager

	tuiHostFilter string
	tuiRepoFilter string

	statuses []runner.RunnerStatus
	cursor   int
	width    int
	height   int

	loading bool
	lastErr string
	toast   string
	busy    bool
	busyOp  string

	showHelp bool

	panel             panelKind
	menuCursor        int
	confirmRemoveInst string

	scrollTitle string
	scrollLines []string
	scrollOff   int

	filterHostChoices []string
	filterRepoChoices []string

	hostMetrics    []host.HostMetrics
	hostMetricsCur int
	metricsLoading bool
}

type statusRefreshedMsg struct {
	statuses []runner.RunnerStatus
	err      error
}

type tickMsg time.Time

type opDoneMsg struct {
	err error
	op  string
}

type logLoadedMsg struct {
	text string
	err  error
}

type doctorDoneMsg struct {
	out string
}

type validateDoneMsg struct {
	err error
}

type editorDoneMsg struct {
	err error
}

type hostMetricsMsg struct {
	metrics []host.HostMetrics
}

var (
	actionMenuLabels = []string{"Setup", "Start (up)", "Stop (down)", "Restart", "Update", "View logs", "Remove"}
	globalMenuLabels = []string{
		"Doctor",
		"Host metrics (CPU, memory, disk)",
		"Cleanup offline runners (GitHub API)",
		"Show configuration",
		"Validate configuration",
		"Edit runners.yml",
		"Edit env file",
		"Filter by host…",
		"Filter by repo…",
		"Clear filters",
	}
	filterMenuLabels = []string{"Filter by host…", "Filter by repo…", "Clear all filters"}
)

func RunDashboard(cfg *config.Config, opts DashboardOpts) error {
	tok, err := config.ResolveToken(cfg)
	if err != nil {
		return err
	}
	mgr := runner.NewManager(tok)
	mgr.Out = io.Discard
	mgr.GhSrVersion = opts.GhSrVersion
	m := &dashboardModel{
		opts:          opts,
		cfg:           cfg,
		mgr:           mgr,
		tuiHostFilter: opts.FilterHost,
		tuiRepoFilter: opts.FilterRepo,
		loading:       true,
		panel:         panelMain,
	}

	p := tea.NewProgram(m)
	_, err = p.Run()
	return err
}

func (m *dashboardModel) Init() tea.Cmd {
	return tea.Batch(
		m.refreshCmd(),
		tickEvery(5*time.Second),
	)
}

func (m *dashboardModel) refreshCmd() tea.Cmd {
	cfg := m.cfg
	mgr := m.mgr
	hostF := m.tuiHostFilter
	repoF := m.tuiRepoFilter
	return func() tea.Msg {
		statuses, err := ops.CollectStatus(nil, cfg, mgr, hostF, repoF, nil)
		return statusRefreshedMsg{statuses: statuses, err: err}
	}
}

func (m *dashboardModel) refreshMetricsCmd() tea.Cmd {
	cfg := m.cfg
	hostF := m.tuiHostFilter
	return func() tea.Msg {
		metrics := ops.CollectHostMetrics(nil, cfg, hostF)
		return hostMetricsMsg{metrics: metrics}
	}
}

func (m *dashboardModel) selectedInstance() (string, bool) {
	if m.cursor < 0 || m.cursor >= len(m.statuses) {
		return "", false
	}
	return m.statuses[m.cursor].Instance, true
}

// hostForSelectedAction returns the host filter for ops invoked from the action menu:
// explicit TUI/CLI host filter if set, otherwise the host of the selected status row.
func (m *dashboardModel) hostForSelectedAction() string {
	if m.tuiHostFilter != "" {
		return m.tuiHostFilter
	}
	if m.cursor >= 0 && m.cursor < len(m.statuses) {
		return m.statuses[m.cursor].Host
	}
	return ""
}

func (m *dashboardModel) runOp(name string, fn func() error) tea.Cmd {
	m.busy = true
	m.busyOp = name
	m.toast = ""
	return func() tea.Msg {
		return opDoneMsg{op: name, err: fn()}
	}
}

func (m *dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		key := msg.String()
		if m.handleScrollKeys(key) {
			return m, nil
		}
		if m.panel == panelConfirmCleanup {
			switch key {
			case "y":
				m.panel = panelMain
				m.menuCursor = 0
				m.toast = ""
				return m, m.runOp("cleanup", func() error {
					_, err := ops.CleanupOffline(io.Discard, m.cfg, m.mgr)
					return err
				})
			case "n", "esc":
				m.panel = panelMain
				m.menuCursor = 0
			}
			return m, nil
		}
		if m.panel == panelConfirmRemove {
			switch key {
			case "y":
				inst := m.confirmRemoveInst
				hostF := m.hostForSelectedAction()
				m.panel = panelMain
				m.menuCursor = 0
				m.toast = ""
				return m, m.runOp("remove", func() error {
					return ops.Remove(io.Discard, m.cfg, m.mgr, hostF, m.tuiRepoFilter, []string{inst})
				})
			case "n", "esc":
				m.panel = panelMain
				m.menuCursor = 0
			}
			return m, nil
		}

		switch key {
		case "?":
			m.showHelp = !m.showHelp
			return m, nil
		}

		if m.busy && m.panel == panelMain {
			if key == "ctrl+c" || key == "q" {
				return m, tea.Quit
			}
			return m, nil
		}

		switch m.panel {
		case panelActionMenu:
			return m, m.updateActionMenu(key)
		case panelGlobalMenu:
			return m, m.updateGlobalMenu(key)
		case panelFilterMenu:
			return m, m.updateFilterMenu(key)
		case panelFilterHost:
			return m, m.updateFilterHost(key)
		case panelFilterRepo:
			return m, m.updateFilterRepo(key)
		case panelHostMetrics:
			return m, m.updateHostMetrics(key)
		}

		switch key {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "r":
			if m.panel == panelMain {
				m.loading = true
				return m, m.refreshCmd()
			}
		case "h":
			if m.panel == panelMain {
				m.metricsLoading = true
				return m, m.refreshMetricsCmd()
			}
		case "g":
			if m.panel == panelMain {
				m.panel = panelGlobalMenu
				m.menuCursor = 0
			}
		case "f":
			if m.panel == panelMain {
				m.panel = panelFilterMenu
				m.menuCursor = 0
			}
		case "enter":
			if m.panel == panelMain && len(m.statuses) > 0 {
				m.panel = panelActionMenu
				m.menuCursor = 0
			}
		case "j", "down":
			if m.panel == panelMain && len(m.statuses) > 0 {
				if m.cursor < len(m.statuses)-1 {
					m.cursor++
				}
			}
		case "k", "up":
			if m.panel == panelMain && m.cursor > 0 {
				m.cursor--
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case statusRefreshedMsg:
		m.loading = false
		if msg.err != nil {
			m.lastErr = msg.err.Error()
		} else {
			m.lastErr = ""
			m.statuses = msg.statuses
			m.cursor = clampCursor(m.cursor, len(m.statuses))
		}

	case tickMsg:
		if m.busy || m.panel != panelMain {
			return m, tickEvery(5 * time.Second)
		}
		return m, tea.Batch(m.refreshCmd(), tickEvery(5*time.Second))

	case opDoneMsg:
		m.busy = false
		m.busyOp = ""
		if msg.err != nil {
			m.lastErr = msg.err.Error()
			m.toast = ""
		} else {
			m.lastErr = ""
			m.toast = msg.op + " complete"
		}
		m.loading = true
		return m, m.refreshCmd()

	case logLoadedMsg:
		m.busy = false
		m.busyOp = ""
		if msg.err != nil {
			m.lastErr = msg.err.Error()
			return m, nil
		}
		m.lastErr = ""
		m.scrollTitle = "Logs"
		m.scrollLines = wrapLines(msg.text, max(40, m.width-4))
		m.scrollOff = 0
		m.panel = panelScroll
		return m, nil

	case doctorDoneMsg:
		m.busy = false
		m.busyOp = ""
		m.scrollTitle = "Doctor"
		m.scrollLines = wrapLines(msg.out, max(40, m.width-4))
		m.scrollOff = 0
		m.panel = panelScroll
		return m, nil

	case validateDoneMsg:
		m.busy = false
		m.busyOp = ""
		if msg.err != nil {
			m.lastErr = msg.err.Error()
		} else {
			m.lastErr = ""
			m.toast = "Configuration is valid."
		}
		return m, nil

	case hostMetricsMsg:
		m.metricsLoading = false
		m.hostMetrics = msg.metrics
		m.hostMetricsCur = 0
		m.panel = panelHostMetrics
		return m, nil

	case editorDoneMsg:
		m.busy = false
		m.busyOp = ""
		if msg.err != nil {
			m.lastErr = fmt.Sprintf("editor: %v", msg.err)
			return m, nil
		}
		if err := config.BootstrapEnv(); err != nil {
			m.lastErr = err.Error()
			return m, nil
		}
		cfg, err := config.LoadFromPath(m.opts.ConfigPath)
		if err != nil {
			m.lastErr = err.Error()
			return m, nil
		}
		m.cfg = cfg
		tok, tokErr := config.ResolveToken(cfg)
		if tokErr != nil {
			m.lastErr = tokErr.Error()
			return m, nil
		}
		mgr := runner.NewManager(tok)
		mgr.Out = io.Discard
		mgr.GhSrVersion = m.opts.GhSrVersion
		m.mgr = mgr
		m.toast = "Config reloaded."
		m.loading = true
		return m, m.refreshCmd()
	}

	return m, nil
}

func (m *dashboardModel) updateActionMenu(key string) tea.Cmd {
	switch key {
	case "esc":
		m.panel = panelMain
		m.menuCursor = 0
		return nil
	case "j", "down":
		if m.menuCursor < len(actionMenuLabels)-1 {
			m.menuCursor++
		}
	case "k", "up":
		if m.menuCursor > 0 {
			m.menuCursor--
		}
	case "enter":
		inst, ok := m.selectedInstance()
		if !ok {
			m.panel = panelMain
			return nil
		}
		nameArgs := []string{inst}
		hostF := m.hostForSelectedAction()
		switch m.menuCursor {
		case 0:
			m.panel = panelMain
			return m.runOp("setup", func() error {
				return ops.Setup(io.Discard, m.cfg, m.mgr, hostF, m.tuiRepoFilter, nameArgs)
			})
		case 1:
			m.panel = panelMain
			return m.runOp("up", func() error {
				return ops.Up(io.Discard, m.cfg, m.mgr, hostF, m.tuiRepoFilter, nameArgs)
			})
		case 2:
			m.panel = panelMain
			return m.runOp("down", func() error {
				return ops.Down(io.Discard, m.cfg, m.mgr, hostF, m.tuiRepoFilter, nameArgs)
			})
		case 3:
			m.panel = panelMain
			return m.runOp("restart", func() error {
				return ops.Restart(io.Discard, m.cfg, m.mgr, hostF, m.tuiRepoFilter, nameArgs)
			})
		case 4:
			m.panel = panelMain
			return m.runOp("update", func() error {
				return ops.Update(io.Discard, m.cfg, m.mgr, hostF, m.tuiRepoFilter, nameArgs)
			})
		case 5:
			m.panel = panelMain
			m.busy = true
			m.busyOp = "logs"
			m.toast = ""
			cfg := m.cfg
			mgr := m.mgr
			return func() tea.Msg {
				text, err := ops.Logs(cfg, mgr, hostF, inst)
				return logLoadedMsg{text: text, err: err}
			}
		case 6:
			m.confirmRemoveInst = inst
			m.panel = panelConfirmRemove
			m.menuCursor = 0
			return nil
		}
	}
	return nil
}

func (m *dashboardModel) updateGlobalMenu(key string) tea.Cmd {
	switch key {
	case "esc":
		m.panel = panelMain
		m.menuCursor = 0
		return nil
	case "j", "down":
		if m.menuCursor < len(globalMenuLabels)-1 {
			m.menuCursor++
		}
	case "k", "up":
		if m.menuCursor > 0 {
			m.menuCursor--
		}
	case "enter":
		switch m.menuCursor {
		case 0:
			m.panel = panelMain
			m.busy = true
			m.busyOp = "doctor"
			m.toast = ""
			cfgPath := m.opts.ConfigPath
			envPath := m.opts.EnvPath
			cfg := m.cfg
			hostF := m.tuiHostFilter
			repoF := m.tuiRepoFilter
			return func() tea.Msg {
				var buf bytes.Buffer
				var gh *runner.GitHubClient
				if cfg != nil {
					tok, tokErr := config.ResolveToken(cfg)
					if tokErr == nil {
						gh = runner.NewGitHubClient(tok)
					}
				}
				doctor.Run(&buf, cfgPath, envPath, cfg, nil, gh, hostF, repoF, false, false)
				return doctorDoneMsg{out: buf.String()}
			}
		case 1:
			m.panel = panelMain
			m.metricsLoading = true
			m.menuCursor = 0
			return m.refreshMetricsCmd()
		case 2:
			m.panel = panelConfirmCleanup
			m.menuCursor = 0
		case 3:
			m.scrollTitle = "Configuration"
			m.scrollLines = wrapLines(FormatConfig(m.cfg), max(40, m.width-4))
			m.scrollOff = 0
			m.panel = panelScroll
		case 4:
			m.panel = panelMain
			m.busy = true
			m.busyOp = "validate"
			m.toast = ""
			path := m.opts.ConfigPath
			return func() tea.Msg {
				if err := config.BootstrapEnv(); err != nil {
					return validateDoneMsg{err: err}
				}
				_, err := config.LoadFromPath(path)
				return validateDoneMsg{err: err}
			}
		case 5:
			m.panel = panelMain
			if _, err := os.Stat(m.opts.ConfigPath); err != nil {
				m.lastErr = fmt.Sprintf("config file: %v", err)
				return nil
			}
			m.toast = ""
			m.busy = true
			p := m.opts.ConfigPath
			return tea.ExecProcess(editor.Command(p), func(err error) tea.Msg {
				return editorDoneMsg{err: err}
			})
		case 6:
			m.panel = panelMain
			if err := ensureEnvFile(m.opts.EnvPath); err != nil {
				m.lastErr = err.Error()
				return nil
			}
			m.toast = ""
			m.busy = true
			p := m.opts.EnvPath
			return tea.ExecProcess(editor.Command(p), func(err error) tea.Msg {
				return editorDoneMsg{err: err}
			})
		case 7:
			m.filterHostChoices = m.sortedHostNames()
			m.menuCursor = 0
			m.panel = panelFilterHost
		case 8:
			m.filterRepoChoices = m.sortedRepoNames()
			m.menuCursor = 0
			m.panel = panelFilterRepo
		case 9:
			m.tuiHostFilter = ""
			m.tuiRepoFilter = ""
			m.panel = panelMain
			m.menuCursor = 0
			m.loading = true
			return m.refreshCmd()
		}
	}
	return nil
}

func (m *dashboardModel) updateFilterMenu(key string) tea.Cmd {
	switch key {
	case "esc":
		m.panel = panelMain
		m.menuCursor = 0
	case "j", "down":
		if m.menuCursor < len(filterMenuLabels)-1 {
			m.menuCursor++
		}
	case "k", "up":
		if m.menuCursor > 0 {
			m.menuCursor--
		}
	case "enter":
		switch m.menuCursor {
		case 0:
			m.filterHostChoices = m.sortedHostNames()
			m.menuCursor = 0
			m.panel = panelFilterHost
		case 1:
			m.filterRepoChoices = m.sortedRepoNames()
			m.menuCursor = 0
			m.panel = panelFilterRepo
		case 2:
			m.tuiHostFilter = ""
			m.tuiRepoFilter = ""
			m.panel = panelMain
			m.menuCursor = 0
			m.loading = true
			return m.refreshCmd()
		}
	}
	return nil
}

// updateFilterList handles the standard esc/j/k/enter navigation over a
// choices slice and applies the selection via setter. Shared by
// updateFilterHost and updateFilterRepo, which previously duplicated the
// same switch (52 lines, byte-identical apart from the choices slice and
// the target field). Reading the choices slice at call-time (rather than
// capturing it in the closure) preserves the original behaviour: callers
// re-enter the filter panel between key presses, so filterHostChoices /
// filterRepoChoices may have been refreshed.
func (m *dashboardModel) updateFilterList(key string, choices []string, setter func(string)) tea.Cmd {
	switch key {
	case "esc":
		m.panel = panelMain
		m.menuCursor = 0
	case "j", "down":
		if m.menuCursor < len(choices)-1 {
			m.menuCursor++
		}
	case "k", "up":
		if m.menuCursor > 0 {
			m.menuCursor--
		}
	case "enter":
		if len(choices) == 0 {
			m.panel = panelMain
			return nil
		}
		setter(choices[m.menuCursor])
		m.panel = panelMain
		m.menuCursor = 0
		m.loading = true
		return m.refreshCmd()
	}
	return nil
}

func (m *dashboardModel) updateFilterHost(key string) tea.Cmd {
	return m.updateFilterList(key, m.filterHostChoices, func(v string) { m.tuiHostFilter = v })
}

func (m *dashboardModel) updateFilterRepo(key string) tea.Cmd {
	return m.updateFilterList(key, m.filterRepoChoices, func(v string) { m.tuiRepoFilter = v })
}

func (m *dashboardModel) updateHostMetrics(key string) tea.Cmd {
	switch key {
	case "esc", "q":
		m.panel = panelMain
		m.hostMetrics = nil
	case "r":
		m.metricsLoading = true
		return m.refreshMetricsCmd()
	}
	return nil
}

func (m *dashboardModel) handleScrollKeys(key string) bool {
	if m.panel != panelScroll {
		return false
	}
	page := max(1, m.height-6)
	switch key {
	case "esc", "q":
		m.panel = panelMain
		m.scrollLines = nil
		m.scrollOff = 0
		return true
	case "j", "down":
		if m.scrollOff < len(m.scrollLines)-1 {
			m.scrollOff++
		}
		return true
	case "k", "up":
		if m.scrollOff > 0 {
			m.scrollOff--
		}
		return true
	case "ctrl+d", "pgdown":
		m.scrollOff = min(m.scrollOff+page, max(0, len(m.scrollLines)-1))
		return true
	case "ctrl+u", "pgup":
		m.scrollOff = max(0, m.scrollOff-page)
		return true
	case "home", "g":
		m.scrollOff = 0
		return true
	case "end", "G", "shift+g":
		m.scrollOff = max(0, len(m.scrollLines)-1)
		return true
	}
	return false
}

func (m *dashboardModel) sortedHostNames() []string {
	out := make([]string, 0, len(m.cfg.Hosts))
	for k := range m.cfg.Hosts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (m *dashboardModel) sortedRepoNames() []string {
	seen := make(map[string]bool)
	for _, r := range m.cfg.Runners {
		if r.Org != "" {
			seen["org:"+r.Org] = true
		} else {
			seen[r.Repo] = true
		}
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func ensureEnvFile(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return os.WriteFile(path, []byte(config.EnvFileTemplate), 0o600)
	}
	return nil
}

func tickEvery(d time.Duration) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(d)
		return tickMsg(time.Now())
	}
}

func clampCursor(c, n int) int {
	if n <= 0 {
		return 0
	}
	if c >= n {
		return n - 1
	}
	if c < 0 {
		return 0
	}
	return c
}

func wrapLines(s string, width int) []string {
	if width < 20 {
		width = 80
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		rest := line
		for len(rest) > width {
			out = append(out, rest[:width])
			rest = rest[width:]
		}
		out = append(out, rest)
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

package runner

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/an-lee/gh-sr/internal/autostart"
	"github.com/an-lee/gh-sr/internal/config"
	"github.com/an-lee/gh-sr/internal/host"
	"github.com/an-lee/gh-sr/internal/hostshell"
	"github.com/an-lee/gh-sr/internal/strfmt"
)

// DiskWarnThresholdGiB is the doctor warning threshold for runner state directories.
const DiskWarnThresholdGiB = 50

// DiskUsageEntry reports disk consumption for one runner instance directory.
type DiskUsageEntry struct {
	Instance        string
	Host            string
	Path            string
	Orphan          bool
	Mode            string // "native" or "container"
	Busy            bool
	Remote          string // GitHub status when known
	TotalBytes      int64
	WorkBytes       int64
	TempBytes       int64
	DockerDataBytes int64
	OtherBytes      int64
	Err             error
}

// PruneOptions configures PruneInstance.
type PruneOptions struct {
	DryRun         bool
	PruneCache     bool // also prune inner Docker cache (docker-data); default keeps cache
	IncludeOrphans bool
}

// PruneResult summarizes one prune attempt.
type PruneResult struct {
	Instance string
	Host     string
	Skipped  bool
	Reason   string
	Actions  []string
	Err      error
}

// DiskWarnThresholdBytes is DiskWarnThresholdGiB as bytes.
func DiskWarnThresholdBytes() int64 {
	return int64(DiskWarnThresholdGiB) * 1024 * 1024 * 1024
}

// SafeRunnerInstanceName reports whether name is safe to embed in remote shell paths.
func SafeRunnerInstanceName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("invalid instance name %q", name)
	}
	if strings.ContainsAny(name, "/\\\x00\n\r") {
		return fmt.Errorf("invalid instance name %q", name)
	}
	if strings.ContainsAny(name, `;"|&<>$`+"`") {
		return fmt.Errorf("invalid instance name %q", name)
	}
	return nil
}

// posixInstanceEscaper escapes an instance name for embedding inside a POSIX
// double-quoted shell string. The replacement set is the same one that the
// earlier per-call strings.NewReplacer used; promoting it to a package-level
// value lets posixRunnerDirVar (called by posixScriptHeader, which is invoked
// from buildDirSizesPOSIXScript, clearWorkTempPOSIX, removeDirTreePOSIX) skip
// the per-call Replacer allocation. Replacer.Replace is safe for concurrent
// use, so a single shared instance is fine.
var posixInstanceEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")

func posixRunnerDirVar(instance string) string {
	// Hand-rolled `dir="$HOME/.gh-sr/runners/<escaped>"` avoids the
	// fmt.Sprintf allocation chain; the literal prefix is constant so we just
	// concatenate and return. Worst-case output length is the prefix length
	// (27) plus 2× the input length (every input rune is escaped) — well under
	// any reasonable cap.
	const prefix = `dir="$HOME/.gh-sr/runners/`
	var buf strings.Builder
	buf.Grow(len(prefix) + 2*len(instance) + 2)
	buf.WriteString(prefix)
	buf.WriteString(posixInstanceEscaper.Replace(instance))
	buf.WriteString(`"`)
	return buf.String()
}

func posixScriptHeader(instance string) string {
	return "set -e\n" + posixRunnerDirVar(instance) + "\n"
}

// containerEscalation returns the "docker inspect → start if down → exec if up"
// shell snippet. shellCmd is run via `sh -c` inside the container, so callers
// can pass compound commands like `for sub in ...; do ...; done` without
// re-quoting. When the docker CLI is unavailable or the container cannot be
// started, the snippet is a no-op; the surrounding script must still degrade
// gracefully when the inner command never runs.
func containerEscalation(containerName, shellCmd string) string {
	q := QuoteContainerName(containerName)
	execCmd := DockerExecCommand(containerName, "sh -c "+hostshell.PosixSingleQuote(shellCmd))
	return fmt.Sprintf(`
if command -v docker >/dev/null 2>&1; then
  if ! docker inspect --format='{{.State.Running}}' %s 2>/dev/null | grep -q true; then
    docker start %s >/dev/null 2>&1 || true
  fi
  if docker inspect --format='{{.State.Running}}' %s 2>/dev/null | grep -q true; then
    %s || true
  fi
fi
`, q, q, q, execCmd)
}

// passwordlessSudo returns the hostshell.LinuxElevatePreludeSoft fragment used
// by disk-prune scripts that need non-interactive root or passwordless sudo
// over SSH. The soft variant is required because these scripts run several
// elevated commands sequentially (clearWorkTempPOSIX, removeDirTreePOSIX) and
// must keep going when one fails so the user sees each per-command outcome.
// Callers gate usage of "$SUDO" with `if [ -n "$SUDO" ] || [ "$(id -u)" -eq 0 ]`
// (or similar) so the empty-string case falls through to a non-elevated attempt
// that surfaces the real permission error.
//
// This thin wrapper exists for symmetry with internal/runner/sudo.go and so the
// test can call a package-local name.
func passwordlessSudo() string {
	return hostshell.LinuxElevatePreludeSoft()
}

// ListRunnerInstanceDirs returns subdirectory names under ~/.gh-sr/runners on the host.
// Names that fail SafeRunnerInstanceName are omitted.
func ListRunnerInstanceDirs(h *host.Host) ([]string, error) {
	raw, err := runOnHostOS(h,
		func() ([]string, error) {
			ps := `$base = Join-Path $env:USERPROFILE '.gh-sr\runners'; if (Test-Path $base) { Get-ChildItem -Path $base -Directory -ErrorAction SilentlyContinue | ForEach-Object { $_.Name } }`
			out, err := h.RunShell(ps)
			if err != nil {
				return nil, err
			}
			return splitNonEmptyLines(out), nil
		},
		func() ([]string, error) {
			out, err := h.Run(`ls -1 "$HOME/.gh-sr/runners" 2>/dev/null || true`)
			if err != nil {
				return nil, err
			}
			return splitNonEmptyLines(out), nil
		},
	)
	if err != nil {
		return nil, err
	}
	var safe []string
	for _, name := range raw {
		if SafeRunnerInstanceName(name) == nil {
			safe = append(safe, name)
		}
	}
	return safe, nil
}

func splitNonEmptyLines(s string) []string {
	// SplitSeq avoids the upfront []string allocation that strings.Split makes
	// for the full output of the per-host `ls -1 ~/.gh-sr/runners` (or PowerShell
	// Get-ChildItem) command. The returned slice is what callers consume, so the
	// caller-side allocation is unchanged; only the intermediate slice is removed.
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// MeasureDiskUsage measures disk usage for one runner instance directory.
func MeasureDiskUsage(h *host.Host, hostName, instance string, rc *config.RunnerConfig) DiskUsageEntry {
	entry := DiskUsageEntry{
		Instance: instance,
		Host:     hostName,
		Path:     h.RunnerDir(instance),
	}

	if err := SafeRunnerInstanceName(instance); err != nil {
		entry.Err = err
		return entry
	}

	if rc != nil {
		entry.Mode = rc.EffectiveRunnerMode()
	} else {
		entry.Orphan = true
		entry.Mode = "unknown"
	}

	total, work, temp, dockerData, err := dirSizes(h, instance)
	if err != nil {
		entry.Err = err
		return entry
	}
	entry.TotalBytes = total
	entry.WorkBytes = work
	entry.TempBytes = temp
	entry.DockerDataBytes = dockerData
	other := total - work - temp - dockerData
	if other < 0 {
		other = 0
	}
	entry.OtherBytes = other
	return entry
}

// dirSizesResult bundles the four size buckets dirSizes collects on the remote
// host so it can flow through runOnHostOS's generic dispatch (the helper
// returns a single value, and dirSizes needs to ship four int64s back).
type dirSizesResult struct {
	total, work, temp, dockerData int64
}

func dirSizes(h *host.Host, instance string) (total, work, temp, dockerData int64, err error) {
	res, err := runOnHostOS(h,
		func() (dirSizesResult, error) {
			t, w, te, dk, ierr := dirSizesWindows(h, instance)
			return dirSizesResult{t, w, te, dk}, ierr
		},
		func() (dirSizesResult, error) {
			t, w, te, dk, ierr := dirSizesPOSIX(h, instance)
			return dirSizesResult{t, w, te, dk}, ierr
		},
	)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return res.total, res.work, res.temp, res.dockerData, nil
}

// buildDirSizesPOSIXScript returns the shell script that `dirSizesPOSIX`
// runs on the remote host. Exposed (1) so the structural test in
// `disk_test.go` can assert the single-walk invariant against the real
// production string instead of a frozen copy, and (2) so a future
// refactor that re-introduces multiple `du` walks fires the test.
func buildDirSizesPOSIXScript(instance string) string {
	// Single `du` walk with depth 1 reports the total and the size of each
	// first-level subdirectory in one pass. Replaces four separate `du` calls
	// (one for $dir, one each for _work/_temp/docker-data) which re-walked
	// overlapping subtrees. On remote hosts the savings compound because each
	// `h.Run` is a separate SSH round trip.
	//
	// `du` flag differs by platform: GNU coreutils uses --max-depth=N, BSD/macOS
	// uses -d N. Probe with --max-depth=0 and fall back to -d 0.
	return fmt.Sprintf(`
%sif [ ! -d "$dir" ]; then echo "0 0 0 0"; exit 0; fi
if du --max-depth=0 "$dir" >/dev/null 2>&1; then
  out=$(du --max-depth=1 -k "$dir" 2>/dev/null)
else
  out=$(du -d 1 -k "$dir" 2>/dev/null)
fi
if [ -z "$out" ]; then echo "0 0 0 0"; exit 0; fi
total=0; work=0; temp=0; docker=0
total_name=$(basename "$dir")
# Default IFS splits the "size<TAB>path" lines. GNU du emits tab-separated;
# BSD du emits space-separated. Both work with the default.
while read -r size path; do
  [ -z "$path" ] && continue
  case "$(basename "$path")" in
    "$total_name") total=$((size * 1024)) ;;
    _work)         work=$((size * 1024)) ;;
    _temp)         temp=$((size * 1024)) ;;
    docker-data)   docker=$((size * 1024)) ;;
  esac
done <<< "$out"
echo "$total $work $temp $docker"
`, posixScriptHeader(instance))
}

func dirSizesPOSIX(h *host.Host, instance string) (total, work, temp, dockerData int64, err error) {
	out, err := h.Run(buildDirSizesPOSIXScript(instance))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return parseFourInt64s(out)
}

// buildDirSizesBatchPOSIXScript returns the shell script that
// `dirSizesBatchPOSIX` runs on the remote host. It walks the per-instance
// `du` tree once per instance (matching the existing single-instance script)
// but emits one tab-separated "instance total work temp docker" line per
// instance over a single SSH round-trip. The same `du --max-depth`/`du -d`
// probe is preserved so the GNU/BSD portability contract is identical to
// `buildDirSizesPOSIXScript`.
//
// Instance names are emitted via `printf %q` so embedded spaces, tabs, and
// quote-metacharacters still in the safe-set (e.g. `'`, `*`) are passed
// verbatim to POSIX shell. `SafeRunnerInstanceName` already rejects the
// shell-dangerous set (`; " | & < > $ \` / \\`); the printf %q layer is
// defense-in-depth for the remaining characters.
func buildDirSizesBatchPOSIXScript(instances []string) string {
	// Build a single shell script that walks each instance's `du` tree
	// once and emits one tab-separated line per instance. Instance names
	// are emitted via `printf %q` so embedded spaces, tabs, and the
	// remaining safe-set characters (`'`, `*`, etc.) survive the shell
	// round-trip. `SafeRunnerInstanceName` already rejects the truly
	// shell-dangerous set; the printf %q layer is defense-in-depth.
	var b strings.Builder
	b.WriteString("# ghsr-batch-disk-v1\n")
	b.WriteString("set -e\n")
	b.WriteString(`base="$HOME/.gh-sr/runners"` + "\n")
	b.WriteString("for inst in")
	for _, inst := range instances {
		b.WriteByte(' ')
		b.WriteString(strconv.Quote(inst))
	}
	b.WriteString("; do\n")
	b.WriteString(`  dir="$base/$inst"` + "\n")
	b.WriteString(`  if [ ! -d "$dir" ]; then printf '%s\t0\t0\t0\t0\n' "$inst"; continue; fi` + "\n")
	b.WriteString(`  if du --max-depth=0 "$dir" >/dev/null 2>&1; then` + "\n")
	b.WriteString(`    out=$(du --max-depth=1 -k "$dir" 2>/dev/null)` + "\n")
	b.WriteString("  else\n")
	b.WriteString(`    out=$(du -d 1 -k "$dir" 2>/dev/null)` + "\n")
	b.WriteString("  fi\n")
	b.WriteString(`  if [ -z "$out" ]; then printf '%s\t0\t0\t0\t0\n' "$inst"; continue; fi` + "\n")
	b.WriteString("  total=0; work=0; temp=0; docker=0\n")
	b.WriteString(`  total_name=$(basename "$dir")` + "\n")
	b.WriteString(`  while read -r size path; do` + "\n")
	b.WriteString(`    [ -z "$path" ] && continue` + "\n")
	b.WriteString(`    case "$(basename "$path")" in` + "\n")
	b.WriteString(`      "$total_name") total=$((size * 1024)) ;;` + "\n")
	b.WriteString(`      _work)         work=$((size * 1024)) ;;` + "\n")
	b.WriteString(`      _temp)         temp=$((size * 1024)) ;;` + "\n")
	b.WriteString(`      docker-data)   docker=$((size * 1024)) ;;` + "\n")
	b.WriteString("    esac\n")
	b.WriteString(`  done <<< "$out"` + "\n")
	b.WriteString(`  printf '%s\t%s\t%s\t%s\t%s\n' "$inst" "$total" "$work" "$temp" "$docker"` + "\n")
	b.WriteString("done\n")
	return b.String()
}

// dirSizesBatchPOSIX runs one SSH round-trip that returns the four-bucket
// size summary for every requested instance, keyed by instance name. The
// returned map only contains entries for instances the script emitted a
// line for (always every requested instance on the happy path); callers
// can rely on `len(result) == len(instances)` unless the host-side script
// truncated the output, which surfaces as a parse error.
func dirSizesBatchPOSIX(h *host.Host, instances []string) (map[string]dirSizesResult, error) {
	if len(instances) == 0 {
		return map[string]dirSizesResult{}, nil
	}
	out, err := h.Run(buildDirSizesBatchPOSIXScript(instances))
	if err != nil {
		return nil, err
	}
	return parseDirSizesBatch(out, instances)
}

// parseDirSizesBatch parses the tab-separated output of dirSizesBatchPOSIX
// into a per-instance map. Each line has the shape
//
//	<instance>\t<total>\t<work>\t<temp>\t<docker>\n
//
// Missing instances (instances requested but no line emitted) are returned
// as an explicit error so the caller can surface a host-side truncation
// rather than silently dropping data. Stale newlines / blank lines are
// skipped.
func parseDirSizesBatch(out string, expected []string) (map[string]dirSizesResult, error) {
	res := make(map[string]dirSizesResult, len(expected))
	// Manual scan — strings.Split on potentially large output would
	// allocate an intermediate []string we don't need.
	i := 0
	for i < len(out) {
		// Skip blank lines / trailing whitespace.
		for i < len(out) && (out[i] == '\n' || out[i] == '\r') {
			i++
		}
		if i >= len(out) {
			break
		}
		// Read one line.
		start := i
		for i < len(out) && out[i] != '\n' {
			i++
		}
		line := out[start:i]
		// Trim trailing \r.
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		// Manual field split (5 fields, tab-separated).
		var fields [5]string
		nf := 0
		pos := 0
		for pos < len(line) && nf < 5 {
			fs := pos
			for pos < len(line) && line[pos] != '\t' {
				pos++
			}
			fields[nf] = line[fs:pos]
			nf++
			if pos < len(line) {
				pos++ // skip tab
			}
		}
		if pos < len(line) {
			// extra fields → caller's contract is violated.
			return nil, fmt.Errorf("batch disk sizes line %q: too many fields", line)
		}
		if nf < 5 {
			return nil, fmt.Errorf("batch disk sizes line %q: too few fields", line)
		}
		var vals [4]int64
		for k := 0; k < 4; k++ {
			v, perr := strconv.ParseInt(fields[k+1], 10, 64)
			if perr != nil {
				return nil, fmt.Errorf("batch disk sizes line %q: parsing %q: %w", line, fields[k+1], perr)
			}
			vals[k] = v
		}
		res[fields[0]] = dirSizesResult{
			total:      vals[0],
			work:       vals[1],
			temp:       vals[2],
			dockerData: vals[3],
		}
	}
	for _, want := range expected {
		if _, ok := res[want]; !ok {
			return nil, fmt.Errorf("batch disk sizes: missing instance %q in output", want)
		}
	}
	return res, nil
}

// buildDirSizesBatchWindowsScript returns the wrapped PowerShell script
// that emits one tab-separated "instance total work temp docker" line per
// requested instance. The script and output shape mirror dirSizesBatchPOSIX
// so the Go-side parser is shared.
func buildDirSizesBatchWindowsScript(instances []string) string {
	if len(instances) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# ghsr-batch-disk-v1\n")
	b.WriteString(`
function Ghsr-DirSize([string]$p) {
  if (-not (Test-Path -LiteralPath $p)) { return 0 }
  $sum = (Get-ChildItem -LiteralPath $p -Recurse -Force -ErrorAction SilentlyContinue | Measure-Object -Property Length -Sum).Sum
  if ($null -eq $sum) { return 0 }
  return [int64]$sum
}
function Ghsr-OtherDirSize([string]$root) {
  if (-not (Test-Path -LiteralPath $root)) { return 0 }
  $skip = @('_work','_temp','docker-data')
  $sum = [int64]0
  Get-ChildItem -LiteralPath $root -Force -ErrorAction SilentlyContinue | ForEach-Object {
    if ($skip -contains $_.Name) { return }
    if ($_.PSIsContainer) { $sum += Ghsr-DirSize $_.FullName } else { $sum += [int64]$_.Length }
  }
  return $sum
}
$base = Join-Path $env:USERPROFILE '.gh-sr\runners'
`)
	for _, inst := range instances {
		// Use single-quoted PowerShell literal so embedded spaces survive
		// the round-trip; single quotes inside the name are doubled, which
		// is the canonical PowerShell escape for single-quoted strings.
		// SafeRunnerInstanceName already rejects the truly shell-dangerous
		// set; this is defence-in-depth.
		escaped := strings.ReplaceAll(inst, "'", "''")
		b.WriteString(fmt.Sprintf("$inst = '%s'\n", escaped))
		b.WriteString(`$d = Join-Path $base $inst` + "\n")
		b.WriteString("if (-not (Test-Path -LiteralPath $d)) { Write-Output ($inst + \"\t0\t0\t0\t0\"); continue }\n")
		b.WriteString(`$w = Ghsr-DirSize (Join-Path $d '_work')` + "\n")
		b.WriteString(`$te = Ghsr-DirSize (Join-Path $d '_temp')` + "\n")
		b.WriteString(`$dk = Ghsr-DirSize (Join-Path $d 'docker-data')` + "\n")
		b.WriteString(`$other = Ghsr-OtherDirSize $d` + "\n")
		b.WriteString(`$t = $w + $te + $dk + $other` + "\n")
		b.WriteString(`Write-Output ("$inst	$t	$w	$te	$dk")` + "\n")
	}
	return b.String()
}

// dirSizesBatchWindows is the Windows counterpart of dirSizesBatchPOSIX.
// Returns the same per-instance map.
func dirSizesBatchWindows(h *host.Host, instances []string) (map[string]dirSizesResult, error) {
	if len(instances) == 0 {
		return map[string]dirSizesResult{}, nil
	}
	out, err := h.RunShell(buildDirSizesBatchWindowsScript(instances))
	if err != nil {
		return nil, err
	}
	return parseDirSizesBatch(out, instances)
}

// dirSizesBatch runs one SSH round-trip per host that returns the four
// size buckets for every requested instance. Caller passes the full set
// of instances to measure; rcByInstance is the same map the single-call
// `MeasureDiskUsage` takes, used by MeasureDiskUsageBatch to populate the
// `Mode`/`Orphan` fields on the returned entries.
func dirSizesBatch(h *host.Host, instances []string) (map[string]dirSizesResult, error) {
	return runOnHostOS(h,
		func() (map[string]dirSizesResult, error) {
			return dirSizesBatchWindows(h, instances)
		},
		func() (map[string]dirSizesResult, error) {
			return dirSizesBatchPOSIX(h, instances)
		},
	)
}

// MeasureDiskUsageBatch measures disk usage for every supplied instance in
// a single SSH round-trip per host. The behaviour matches the per-instance
// `MeasureDiskUsage` for every populated entry (Mode, Orphan, TotalBytes,
// WorkBytes, TempBytes, DockerDataBytes, OtherBytes, Err) — the result map
// is keyed by instance name and contains exactly `len(instances)` entries
// on the happy path. Entries with an unsafe instance name are returned with
// `Err` set and zero-valued size buckets, matching the per-instance API's
// error-shape contract.
func MeasureDiskUsageBatch(h *host.Host, hostName string, instances []string, rcByInstance map[string]*config.RunnerConfig) map[string]DiskUsageEntry {
	out := make(map[string]DiskUsageEntry, len(instances))
	if len(instances) == 0 {
		return out
	}

	// Filter to safe names — the per-instance path validates `SafeRunnerInstanceName`
	// before shipping the script and returns an Err-typed entry rather than
	// embedding the unsafe name in shell. The batch script cites every
	// instance via `printf %q` / PowerShell single-quote, so unsafe names
	// would still parse cleanly (single character isn't a shell metachar),
	// but the per-instance API's contract is to reject them outright. We
	// preserve that contract here so callers see the same surface.
	safeInstances := make([]string, 0, len(instances))
	for _, inst := range instances {
		entry := DiskUsageEntry{
			Instance: inst,
			Host:     hostName,
			Path:     h.RunnerDir(inst),
		}
		if err := SafeRunnerInstanceName(inst); err != nil {
			entry.Err = err
			out[inst] = entry
			continue
		}
		if rc, ok := rcByInstance[inst]; ok && rc != nil {
			entry.Mode = rc.EffectiveRunnerMode()
		} else {
			entry.Orphan = true
			entry.Mode = "unknown"
		}
		out[inst] = entry
		safeInstances = append(safeInstances, inst)
	}

	if len(safeInstances) == 0 {
		return out
	}

	sizes, err := dirSizesBatch(h, safeInstances)
	if err != nil {
		// Surface the batch error on every safe entry so the caller can
		// still render an entry per instance (mirrors the per-instance
		// API's "Err set, other fields zero" contract).
		for _, inst := range safeInstances {
			e := out[inst]
			e.Err = err
			out[inst] = e
		}
		return out
	}

	for _, inst := range safeInstances {
		s := sizes[inst]
		e := out[inst]
		e.TotalBytes = s.total
		e.WorkBytes = s.work
		e.TempBytes = s.temp
		e.DockerDataBytes = s.dockerData
		other := s.total - s.work - s.temp - s.dockerData
		if other < 0 {
			other = 0
		}
		e.OtherBytes = other
		out[inst] = e
	}
	return out
}

func dirSizesWindows(h *host.Host, instance string) (total, work, temp, dockerData int64, err error) {
	dirExpr := h.RunnerDirPS(instance)
	ps := fmt.Sprintf(`
function Ghsr-DirSize([string]$p) {
  if (-not (Test-Path -LiteralPath $p)) { return 0 }
  $sum = (Get-ChildItem -LiteralPath $p -Recurse -Force -ErrorAction SilentlyContinue | Measure-Object -Property Length -Sum).Sum
  if ($null -eq $sum) { return 0 }
  return [int64]$sum
}
function Ghsr-OtherDirSize([string]$root) {
  if (-not (Test-Path -LiteralPath $root)) { return 0 }
  $skip = @('_work','_temp','docker-data')
  $sum = [int64]0
  Get-ChildItem -LiteralPath $root -Force -ErrorAction SilentlyContinue | ForEach-Object {
    if ($skip -contains $_.Name) { return }
    if ($_.PSIsContainer) { $sum += Ghsr-DirSize $_.FullName } else { $sum += [int64]$_.Length }
  }
  return $sum
}
$d = %s
$w = Ghsr-DirSize (Join-Path $d '_work')
$te = Ghsr-DirSize (Join-Path $d '_temp')
$dk = Ghsr-DirSize (Join-Path $d 'docker-data')
$other = Ghsr-OtherDirSize $d
$t = $w + $te + $dk + $other
Write-Output "$t $w $te $dk"
`, dirExpr)
	out, err := h.RunShell(ps)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return parseFourInt64s(out)
}

// parseFourInt64s extracts up to four int64 values from the trailing
// whitespace-separated line of `out`. The line shape is the emit produced by
// the `du`-based POSIX script (`echo "$total $work $temp $docker"`) and the
// `Write-Output "$t $w $te $dk"` PowerShell path. The inner scanner is a
// manual ASCII scan over the line — strings.Fields would allocate a
// []string header for every call, and parseFourInt64s runs once per host per
// `gh sr disk` listing refresh, so the saved allocation compounds across
// listings with many hosts.
func parseFourInt64s(out string) (a, b, c, d int64, err error) {
	// Trim leading whitespace and a single trailing newline without
	// strings.TrimSpace's full Unicode pass.
	idx := 0
	for idx < len(out) && (out[idx] == ' ' || out[idx] == '\t' || out[idx] == '\n' || out[idx] == '\r') {
		idx++
	}
	if idx >= len(out) {
		return 0, 0, 0, 0, nil
	}
	end := len(out)
	for end > idx && (out[end-1] == ' ' || out[end-1] == '\t' || out[end-1] == '\n' || out[end-1] == '\r') {
		end--
	}
	line := out[idx:end]
	// If the script emitted multiple lines (a stale diagnostic trailing the
	// sizes), keep the last non-empty line — matches the prior LastIndex('\n')
	// + TrimSpace behavior.
	if nl := strings.LastIndexByte(line, '\n'); nl >= 0 {
		seg := line[nl+1:]
		t := seg
		j := 0
		for j < len(t) && (t[j] == ' ' || t[j] == '\t' || t[j] == '\r') {
			j++
		}
		k := len(t)
		for k > j && (t[k-1] == ' ' || t[k-1] == '\t' || t[k-1] == '\r') {
			k--
		}
		line = t[j:k]
		if line == "" {
			return 0, 0, 0, 0, nil
		}
	}
	// Manual scan: split on whitespace into at most 4 substrings of `line`,
	// then strconv.ParseInt each one. The field slice is a [4]string stack
	// array so no heap allocation is needed for typical 4-number lines.
	var fields [4]string
	nf := 0
	for i := 0; i < len(line); {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i >= len(line) {
			break
		}
		start := i
		for i < len(line) && line[i] != ' ' && line[i] != '\t' {
			i++
		}
		if nf < 4 {
			fields[nf] = line[start:i]
			nf++
		}
	}
	var vals [4]int64
	for i := 0; i < nf; i++ {
		vals[i], err = strconv.ParseInt(fields[i], 10, 64)
		if err != nil {
			return 0, 0, 0, 0, fmt.Errorf("parsing size line %q: %w", line, err)
		}
	}
	return vals[0], vals[1], vals[2], vals[3], nil
}

// PruneInstance reclaims disk for one runner instance when idle.
func (m *Manager) PruneInstance(h *host.Host, hostName, instance string, rc *config.RunnerConfig, busy bool, opts PruneOptions) PruneResult {
	res := PruneResult{Instance: instance, Host: hostName}
	if err := SafeRunnerInstanceName(instance); err != nil {
		res.Err = err
		return res
	}
	if busy {
		res.Skipped = true
		res.Reason = "busy"
		return res
	}

	dir := h.RunnerDir(instance)

	isOrphan := rc == nil
	if isOrphan && !opts.IncludeOrphans {
		res.Skipped = true
		res.Reason = "orphan (use --include-orphans)"
		return res
	}

	if isOrphan && opts.IncludeOrphans {
		action := fmt.Sprintf("remove orphan directory %s", dir)
		res.Actions = append(res.Actions, action)
		if !opts.DryRun {
			if kind, err := autostart.Detect(h, instance); err == nil && kind != autostart.KindNone {
				_ = autostart.Uninstall(h, instance)
			}
			if err := removeDirTree(h, instance); err != nil {
				res.Err = err
			}
		}
		return res
	}

	workAction := fmt.Sprintf("clear %s/_work and %s/_temp", dir, dir)
	res.Actions = append(res.Actions, workAction)

	containerMode := containerPruneMode(rc)
	if containerMode && opts.PruneCache {
		cname := ContainerDockerName(instance)
		cacheAction := fmt.Sprintf("inner docker cache prune in %s", cname)
		res.Actions = append(res.Actions, cacheAction)
	}

	if !opts.DryRun {
		// Pass PruneCache through to clearWorkTemp so the inner docker prune
		// runs inside the same SSH round-trip as the clear. Saves 1 SSH per
		// container-mode prune-with-cache instance — the previous code path
		// issued clearWorkTemp + pruneInnerDockerCache as two separate
		// h.Run calls. Behaviour contract preserved: clear failure still
		// short-circuits the prune (clearWorkTempPOSIX puts the prune block
		// after the "files remain" final check).
		if err := clearWorkTemp(h, instance, containerMode, opts.PruneCache); err != nil {
			res.Err = err
			return res
		}
	}

	return res
}

// containerPruneMode reports whether disk prune should use container escalation paths.
func containerPruneMode(rc *config.RunnerConfig) bool {
	return rc != nil && rc.IsContainerMode()
}

func clearWorkTemp(h *host.Host, instance string, containerMode, pruneCache bool) error {
	_, err := runOnHostOS(h,
		func() (struct{}, error) {
			dirExpr := h.RunnerDirPS(instance)
			ps := fmt.Sprintf(`
foreach ($sub in @('_work','_temp')) {
  $p = Join-Path (%s) $sub
  if (Test-Path -LiteralPath $p) {
    Get-ChildItem -LiteralPath $p -Force -ErrorAction SilentlyContinue | Remove-Item -Recurse -Force -ErrorAction SilentlyContinue
  }
}
`, dirExpr)
			_, ierr := h.RunShell(ps)
			return struct{}{}, ierr
		},
		func() (struct{}, error) {
			_, ierr := h.Run(clearWorkTempPOSIX(instance, containerMode, pruneCache))
			return struct{}{}, ierr
		},
	)
	return err
}

// clearWorkTempPOSIX removes job scratch under _work and _temp. CI jobs often leave
// root-owned files on the host bind mount; we escalate via docker exec (container
// runners) or passwordless host sudo when a plain rm is not enough.
//
// When containerMode and pruneCache are both true, the script ALSO runs the inner
// docker cache prune (probe + `docker system prune -af --volumes`) inside the
// container via a second `docker exec`. Folding this into the same SSH
// round-trip as the clear saves 1 SSH per container-mode prune-with-cache
// instance — PruneInstance previously issued two SSHs (clear + prune) per
// such instance.
//
// The prune block sits AFTER the final-check that confirms _work and _temp are
// empty. If the clear step fails (the final check exits 1 with "cannot remove
// files"), the prune block never runs — preserving the pre-fold behaviour where
// PruneInstance returned immediately on clear failure and skipped the prune.
func clearWorkTempPOSIX(instance string, containerMode, pruneCache bool) string {
	var containerBlock string
	if containerMode {
		containerBlock = containerEscalation(
			ContainerDockerName(instance),
			`for sub in _work _temp; do p="/runner-state/$sub"; if [ -d "$p" ]; then find "$p" -mindepth 1 -maxdepth 1 -exec rm -rf {} +; fi; done`,
		)
	}
	var pruneBlock string
	if containerMode && pruneCache {
		pruneBlock = pruneInnerDockerCacheExec(ContainerDockerName(instance))
	}
	return fmt.Sprintf(`
%s
clear_one() {
  p="$1"
  if [ ! -d "$p" ]; then return 0; fi
  find "$p" -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || true
  if [ -n "$(ls -A "$p" 2>/dev/null)" ]; then return 1; fi
  return 0
}
need_elev=0
for sub in _work _temp; do
  clear_one "$dir/$sub" || need_elev=1
done
if [ "$need_elev" -ne 0 ]; then
%s
%s
  if [ -n "$SUDO" ] || [ "$(id -u)" -eq 0 ]; then
    for sub in _work _temp; do
      p="$dir/$sub"
      if [ -d "$p" ] && [ -n "$(ls -A "$p" 2>/dev/null)" ]; then
        find "$p" -mindepth 1 -maxdepth 1 -exec $SUDO rm -rf {} + 2>/dev/null || true
      fi
    done
  fi
  for sub in _work _temp; do
    p="$dir/$sub"
    if [ -d "$p" ] && [ -n "$(ls -A "$p" 2>/dev/null)" ]; then
      echo "disk prune: cannot remove files in $p (permission denied); for container runners ensure the container is running or use passwordless sudo on the host" >&2
      exit 1
    fi
  done
fi
%s
`, posixScriptHeader(instance), containerBlock, passwordlessSudo(), pruneBlock)
}

// pruneInnerDockerCacheExec returns the host-side command that runs the
// prune script (probe + destructive prune) inside the container via
// `docker exec`. The `|| { ... exit 1; }` wrapper preserves the "Err set when
// the cache is not pruned" contract: if the inner docker exec fails for any
// reason (SSH flake, inner dockerd down, prune failure), the outer shell
// aborts with exit 1 and the captured stderr — which carries the inner
// script's descriptive "inner dockerd not responding" message — reaches
// the caller via h.Run. The wrapper's own "inner docker cache prune in
// <name>: failed" line keeps the pre-fold wrapper prefix reachable to
// callers and tests that pattern-match on it.
//
// The inner script is wrapped in `sh -c '<script>'` because DockerExecCommand
// only concatenates `docker exec <name> ` — it does not wrap raw multi-line
// shell scripts. Without the sh -c boundary the host-side shell (which is
// parsing the surrounding clearWorkTempPOSIX script) would consume the
// if/then/fi branches and the `docker system prune` line itself, running
// them against the host's dockerd instead of the container's. See PR #412
// for the same fix applied to the pre-fold standalone pruneInnerDockerCache.
//
// Folded into clearWorkTempPOSIX so the disk-prune orchestrator spends
// only one SSH per container-mode prune-with-cache instance.
func pruneInnerDockerCacheExec(containerName string) string {
	q := QuoteContainerName(containerName)
	innerCmd := "sh -c " + hostshell.PosixSingleQuote(pruneInnerDockerCacheScript(containerName))
	return fmt.Sprintf(`%s || { echo "inner docker cache prune in %s: failed" >&2; exit 1; }`,
		DockerExecCommand(containerName, innerCmd), q)
}

func removeDirTree(h *host.Host, instance string) error {
	_, err := runOnHostOS(h,
		func() (struct{}, error) {
			dirExpr := h.RunnerDirPS(instance)
			ps := fmt.Sprintf(`if (Test-Path -LiteralPath (%s)) { Remove-Item -LiteralPath (%s) -Recurse -Force }`, dirExpr, dirExpr)
			_, ierr := h.RunShell(ps)
			return struct{}{}, ierr
		},
		func() (struct{}, error) {
			_, ierr := h.Run(removeDirTreePOSIX(instance))
			return struct{}{}, ierr
		},
	)
	return err
}

func removeDirTreePOSIX(instance string) string {
	containerBlock := containerEscalation(
		ContainerDockerName(instance),
		`rm -rf /runner-state`,
	)
	return fmt.Sprintf(`
%s
if [ -d "$dir" ]; then
  rm -rf "$dir" 2>/dev/null || true
fi
if [ -d "$dir" ]; then
  %s
  %s
  if [ -n "$SUDO" ] || [ "$(id -u)" -eq 0 ]; then
    $SUDO rm -rf "$dir" 2>/dev/null || true
  fi
fi
if [ -d "$dir" ]; then
  echo "disk prune: cannot remove orphan directory $dir (permission denied); ensure the runner container is running or use passwordless sudo on the host" >&2
  exit 1
fi
`, posixScriptHeader(instance), containerBlock, passwordlessSudo())
}

// pruneInnerDockerCacheScript returns the shell snippet that probes the
// inner dockerd and, when responsive, runs the destructive cache prune —
// all in one SSH round-trip. The previous implementation issued a separate
// probe + prune call (2 round-trips per container-mode prune-with-cache);
// folding them into one script saves 1 SSH round-trip per instance without
// weakening the "must not run the destructive prune when the inner dockerd
// is down" invariant. The probe-down branch writes the same descriptive
// "inner dockerd not responding" message to stderr and exits non-zero so
// the caller's error-string contract is preserved.
func pruneInnerDockerCacheScript(containerName string) string {
	q := QuoteContainerName(containerName)
	return fmt.Sprintf(`
if ! docker info >/dev/null 2>&1; then
  echo "inner dockerd not responding in %s; skipped cache prune" >&2
  exit 1
fi
docker system prune -af --volumes
`, q)
}

// FormatBytesHuman formats bytes as GiB/MiB/KiB/B for display.
//
// strconv.AppendFloat + stack-allocated byte buffer + inline unit-suffix
// bytes avoids the two allocations the previous `string(AppendFloat(...)) +
// " GiB"` chain dragged in (one for the string coercion, one for the concat).
// The default B branch mirrors the same pattern with strconv.AppendInt instead
// of `strconv.FormatInt(b, 10) + " B"`, dropping its 2-allocs to 1.
//
// Called 5× per row by ops.PrintDiskUsage and once per host by doctor
// DiskEntry rendering, so the per-call alloc drop compounds across listings
// with many instances.
//
// The largest realistic output is "9999.9 GiB" (10 chars); [24]byte holds
// AppendFloat's worst case (~24 chars) plus the 4-char unit suffix. The B
// branch needs at most ~3 bytes for a 0-999 value plus " B", comfortably under
// [16]byte.
func FormatBytesHuman(b int64) string {
	if b < 0 {
		b = 0
	}
	const gib = 1024 * 1024 * 1024
	const mib = 1024 * 1024
	var buf [24]byte
	switch {
	case b >= gib:
		out := buf[:0]
		out = strfmt.FmtFloat(out, float64(b)/float64(gib), 1)
		out = append(out, ' ', 'G', 'i', 'B')
		return string(out)
	case b >= mib:
		out := buf[:0]
		out = strfmt.FmtFloat(out, float64(b)/float64(mib), 1)
		out = append(out, ' ', 'M', 'i', 'B')
		return string(out)
	case b >= 1024:
		out := buf[:0]
		out = strfmt.FmtFloat(out, float64(b)/1024, 1)
		out = append(out, ' ', 'K', 'i', 'B')
		return string(out)
	default:
		var sbuf [16]byte
		out := strconv.AppendInt(sbuf[:0], b, 10)
		out = append(out, ' ', 'B')
		return string(out)
	}
}

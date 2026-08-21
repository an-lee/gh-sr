package host

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/an-lee/gh-sr/internal/strfmt"
)

// HostMetrics holds resource usage information for a single host.
type HostMetrics struct {
	Name string

	// CPU usage as a percentage (0–100).
	CPUPercent float64

	// Memory in MiB.
	MemUsedMiB  float64
	MemTotalMiB float64

	// Disk usage for the root (/) or system volume, in GiB.
	DiskUsedGiB  float64
	DiskTotalGiB float64

	// Load average (1, 5, 15 minutes). Zero on Windows.
	Load1  float64
	Load5  float64
	Load15 float64

	// Uptime as a human-readable string, e.g. "3 days, 4:12".
	Uptime string

	// Error is non-nil when metrics could not be collected.
	Err error
}

func (m HostMetrics) MemPercent() float64 {
	if m.MemTotalMiB == 0 {
		return 0
	}
	return m.MemUsedMiB / m.MemTotalMiB * 100
}

func (m HostMetrics) DiskPercent() float64 {
	if m.DiskTotalGiB == 0 {
		return 0
	}
	return m.DiskUsedGiB / m.DiskTotalGiB * 100
}

// LoadStr formats the three load averages as "l1 l5 l15" with 2-decimal
// precision. strconv.AppendFloat + a stack-allocated byte buffer avoids both
// the strings.Builder heap allocation the previous implementation had AND the
// per-call string allocation that strconv.FormatFloat returns. LoadStr is on
// the TUI metrics render path and runs once per host per render; for a
// 10-host panel that's 10 calls per View(), and the cumulative savings
// compound across long dashboard sessions.
//
// The largest realistic output is around 17 chars (e.g. "99.99 99.99 99.99");
// [40]byte holds three AppendFloat outputs plus the 2 separator spaces
// with room to spare. The buffer is big enough that this function never
// allocates on the heap for realistic load averages.
func (m HostMetrics) LoadStr() string {
	if m.Load1 == 0 && m.Load5 == 0 && m.Load15 == 0 {
		return "-"
	}
	var buf [40]byte
	b := strfmt.FmtFloat(buf[:0], m.Load1, 2)
	b = append(b, ' ')
	b = strfmt.FmtFloat(b, m.Load5, 2)
	b = append(b, ' ')
	b = strfmt.FmtFloat(b, m.Load15, 2)
	return string(b)
}

// AppendLoadStr is the builder-style sibling of LoadStr: it appends the
// three-load-average representation ("l1 l5 l15" with 2-decimal precision, or
// "-" when all three are zero) to dst and returns the extended slice. The
// output stays entirely on the caller's dst slice — no heap allocation is
// incurred for the formatted text, which is what FormatHostMetricsTo needs
// to write the load cell directly into its strings.Builder without going
// through a string round-trip.
//
// [40]byte is comfortably larger than the worst-case 17-char output, so the
// typical usage pattern (FormatHostMetricsTo passes a stack-allocated
// scratch buffer) never escapes to the heap.
func (m HostMetrics) AppendLoadStr(dst []byte) []byte {
	if m.Load1 == 0 && m.Load5 == 0 && m.Load15 == 0 {
		return append(dst, '-')
	}
	dst = strfmt.FmtFloat(dst, m.Load1, 2)
	dst = append(dst, ' ')
	dst = strfmt.FmtFloat(dst, m.Load5, 2)
	dst = append(dst, ' ')
	return strfmt.FmtFloat(dst, m.Load15, 2)
}

// CollectMetrics gathers resource usage from the host.
// It runs a single compound shell command to minimise round-trips.
func (h *Host) CollectMetrics() HostMetrics {
	m := HostMetrics{Name: h.Name}

	if h.OS == "windows" {
		m.Err = h.collectMetricsWindows(&m)
	} else {
		m.Err = h.collectMetricsUnix(&m)
	}
	return m
}

// collectMetricsUnix works on Linux and macOS via POSIX / procfs commands.
func (h *Host) collectMetricsUnix(m *HostMetrics) error {
	script := unixMetricsScript(h.OS)
	out, err := h.RunShell(script)
	if err != nil {
		return fmt.Errorf("running metrics script: %w", err)
	}
	return parseUnixMetrics(out, m)
}

// unixMetricsScript returns a compact shell snippet that prints key=value lines.
func unixMetricsScript(hostOS string) string {
	// CPU: sample /proc/stat (Linux) or vm_stat idle% (macOS).
	// Memory: /proc/meminfo (Linux) or vm_stat+sysctl (macOS).
	// Disk: df on /.
	// Load: /proc/loadavg or sysctl.
	// Uptime: uptime -s or uptime.
	if hostOS == "darwin" {
		return `
echo "::GH_SR_METRICS_START::"
# CPU idle from top (macOS)
cpu_idle=$(top -l 2 -n 0 -s 1 2>/dev/null | awk '/^CPU usage:/{idle=$7} END{gsub(/%/,"",idle); print idle}')
echo "cpu_idle=${cpu_idle}"

# Memory (macOS) — page size * pages
page_size=$(sysctl -n hw.pagesize 2>/dev/null || echo 4096)
mem_total_bytes=$(sysctl -n hw.memsize 2>/dev/null || echo 0)
pages_free=$(vm_stat 2>/dev/null | awk '/Pages free:/{gsub(/\./,"",$3); print $3}')
pages_inactive=$(vm_stat 2>/dev/null | awk '/Pages inactive:/{gsub(/\./,"",$3); print $3}')
pages_speculative=$(vm_stat 2>/dev/null | awk '/Pages speculative:/{gsub(/\./,"",$3); print $3}')
mem_free_bytes=$(( (${pages_free:-0} + ${pages_inactive:-0} + ${pages_speculative:-0}) * page_size ))
mem_total_mib=$(( mem_total_bytes / 1048576 ))
mem_used_mib=$(( (mem_total_bytes - mem_free_bytes) / 1048576 ))
echo "mem_total_mib=${mem_total_mib}"
echo "mem_used_mib=${mem_used_mib}"

# Disk
df_line=$(df -g / 2>/dev/null | tail -1)
disk_total=$(echo "$df_line" | awk '{print $2}')
disk_used=$(echo "$df_line" | awk '{print $3}')
echo "disk_total_gib=${disk_total}"
echo "disk_used_gib=${disk_used}"

# Load average
load=$(sysctl -n vm.loadavg 2>/dev/null | tr -d '{}' | awk '{print $1,$2,$3}')
echo "load=${load}"

# Uptime
boot=$(sysctl -n kern.boottime 2>/dev/null | sed 's/.*sec = \([0-9]*\).*/\1/')
now=$(date +%s)
uptime_secs=$(( now - boot ))
days=$(( uptime_secs / 86400 ))
hours=$(( (uptime_secs % 86400) / 3600 ))
mins=$(( (uptime_secs % 3600) / 60 ))
if [ "$days" -gt 0 ]; then
  echo "uptime=${days}d ${hours}h ${mins}m"
else
  echo "uptime=${hours}h ${mins}m"
fi
echo "::GH_SR_METRICS_END::"
`
	}

	// Linux
	return `
echo "::GH_SR_METRICS_START::"
# CPU: two samples of /proc/stat 1s apart
read_cpu() { awk '/^cpu /{print $2+$3+$4,$5,$2+$3+$4+$5+$6+$7+$8}' /proc/stat; }
sample1=$(read_cpu)
sleep 1
sample2=$(read_cpu)
busy1=$(echo "$sample1" | awk '{print $1}')
idle1=$(echo "$sample1" | awk '{print $2}')
busy2=$(echo "$sample2" | awk '{print $1}')
idle2=$(echo "$sample2" | awk '{print $2}')
busy_d=$(( busy2 - busy1 ))
idle_d=$(( idle2 - idle1 ))
total_d=$(( busy_d + idle_d ))
if [ "$total_d" -gt 0 ]; then
  cpu_idle=$(awk "BEGIN{printf \"%.1f\", $idle_d*100/$total_d}")
else
  cpu_idle="0"
fi
echo "cpu_idle=${cpu_idle}"

# Memory
mem_total=$(awk '/^MemTotal:/{print $2}' /proc/meminfo)
mem_avail=$(awk '/^MemAvailable:/{print $2}' /proc/meminfo)
mem_total_mib=$(( mem_total / 1024 ))
mem_used_mib=$(( (mem_total - mem_avail) / 1024 ))
echo "mem_total_mib=${mem_total_mib}"
echo "mem_used_mib=${mem_used_mib}"

# Disk
df_line=$(df -BG / 2>/dev/null | tail -1)
disk_total=$(echo "$df_line" | awk '{gsub(/G/,"",$2); print $2}')
disk_used=$(echo "$df_line" | awk '{gsub(/G/,"",$3); print $3}')
echo "disk_total_gib=${disk_total}"
echo "disk_used_gib=${disk_used}"

# Load
load=$(cat /proc/loadavg 2>/dev/null | awk '{print $1,$2,$3}')
echo "load=${load}"

# Uptime
uptime_secs=$(awk '{print int($1)}' /proc/uptime 2>/dev/null)
days=$(( uptime_secs / 86400 ))
hours=$(( (uptime_secs % 86400) / 3600 ))
mins=$(( (uptime_secs % 3600) / 60 ))
if [ "$days" -gt 0 ]; then
  echo "uptime=${days}d ${hours}h ${mins}m"
else
  echo "uptime=${hours}h ${mins}m"
fi
echo "::GH_SR_METRICS_END::"
`
}

// parseUnixMetrics extracts key=value pairs between the sentinel markers.
func parseUnixMetrics(raw string, m *HostMetrics) error {
	inBlock := false
	// SplitSeq avoids the upfront []string allocation that strings.Split makes
	// for the whole multi-line script output. parseUnixMetrics runs once per
	// host per TUI metric-refresh tick, so the saved slice lands in steady-state
	// GC pressure on long-running dashboards.
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "::GH_SR_METRICS_START::" {
			inBlock = true
			continue
		}
		if line == "::GH_SR_METRICS_END::" {
			break
		}
		if !inBlock {
			continue
		}

		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "cpu_idle":
			idle, _ := strconv.ParseFloat(v, 64)
			m.CPUPercent = 100 - idle
		case "mem_total_mib":
			m.MemTotalMiB, _ = strconv.ParseFloat(v, 64)
		case "mem_used_mib":
			m.MemUsedMiB, _ = strconv.ParseFloat(v, 64)
		case "disk_total_gib":
			m.DiskTotalGiB, _ = strconv.ParseFloat(v, 64)
		case "disk_used_gib":
			m.DiskUsedGiB, _ = strconv.ParseFloat(v, 64)
		case "load":
			// Parse "l1 l5 l15" as three float64 without allocating a
			// []string. Two strings.IndexByte calls locate the two
			// separator spaces; the three ParseFloat inputs are sub-slices
			// of v (no copy), and ParseFloat does not allocate on success.
			// parseUnixMetrics runs once per host per TUI metric-refresh
			// tick; this is the only heap allocation in the function, so
			// removing it keeps the per-tick parse zero-alloc.
			first := strings.IndexByte(v, ' ')
			if first < 0 {
				continue
			}
			rest := v[first+1:]
			second := strings.IndexByte(rest, ' ')
			if second < 0 {
				continue
			}
			m.Load1, _ = strconv.ParseFloat(v[:first], 64)
			m.Load5, _ = strconv.ParseFloat(rest[:second], 64)
			m.Load15, _ = strconv.ParseFloat(rest[second+1:], 64)
		case "uptime":
			m.Uptime = v
		}
	}
	return nil
}

// collectMetricsWindows uses PowerShell to gather host metrics.
func (h *Host) collectMetricsWindows(m *HostMetrics) error {
	script := windowsMetricsScript()
	out, err := h.RunShell(script)
	if err != nil {
		return fmt.Errorf("running metrics script: %w", err)
	}
	return parseUnixMetrics(out, m) // same key=value format
}

func windowsMetricsScript() string {
	return `
Write-Output "::GH_SR_METRICS_START::"
# CPU
$cpu = (Get-CimInstance Win32_Processor | Measure-Object -Property LoadPercentage -Average).Average
$cpuIdle = 100 - $cpu
Write-Output "cpu_idle=$cpuIdle"

# Memory
$os = Get-CimInstance Win32_OperatingSystem
$memTotalMiB = [math]::Round($os.TotalVisibleMemorySize / 1024)
$memUsedMiB = [math]::Round(($os.TotalVisibleMemorySize - $os.FreePhysicalMemory) / 1024)
Write-Output "mem_total_mib=$memTotalMiB"
Write-Output "mem_used_mib=$memUsedMiB"

# Disk (C:)
$disk = Get-CimInstance Win32_LogicalDisk -Filter "DeviceID='C:'"
$diskTotalGiB = [math]::Round($disk.Size / 1073741824)
$diskUsedGiB = [math]::Round(($disk.Size - $disk.FreeSpace) / 1073741824)
Write-Output "disk_total_gib=$diskTotalGiB"
Write-Output "disk_used_gib=$diskUsedGiB"

# No load average on Windows
Write-Output "load=0 0 0"

# Uptime
$boot = (Get-CimInstance Win32_OperatingSystem).LastBootUpTime
$span = (Get-Date) - $boot
$d = $span.Days; $h = $span.Hours; $min = $span.Minutes
if ($d -gt 0) { Write-Output "uptime=${d}d ${h}h ${min}m" } else { Write-Output "uptime=${h}h ${min}m" }
Write-Output "::GH_SR_METRICS_END::"
`
}

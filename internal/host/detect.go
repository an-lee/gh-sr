package host

import (
	"fmt"
	"strings"

	"github.com/an-lee/gh-sr/internal/hostshell/ps"
)

// probeWindowsShell runs cmd on h via powershell.exe, falling back to
// pwsh.exe if the primary probe fails or its trimmed output does not
// satisfy match. Returns the matched trimmed stdout and true on the
// first probe that satisfies match; returns ("", false) when both probes
// fail or neither probe's output satisfies match. Centralises the
// powershell.exe → pwsh.exe fallback that DetectOS and DetectArch both
// need — keeping the duplication from drifting (see PR #430 for the
// scheduled-task analogue in hostshell).
func probeWindowsShell(h *Host, cmd string, match func(string) bool) (string, bool) {
	out, err := h.Run(ps.CommandLine(cmd))
	if err == nil {
		trimmed := strings.TrimSpace(out)
		if match(trimmed) {
			return trimmed, true
		}
	}
	out, err = h.Run(`pwsh.exe -NoProfile -NonInteractive -Command "` + cmd + `"`)
	if err == nil {
		trimmed := strings.TrimSpace(out)
		if match(trimmed) {
			return trimmed, true
		}
	}
	return "", false
}

// DetectOS probes the remote host for its operating system and returns "linux", "darwin", or "windows".
func DetectOS(h *Host) (string, error) {
	out, err := h.Run(`uname -s 2>/dev/null || echo UNKNOWN`)
	if err == nil {
		switch strings.TrimSpace(strings.ToLower(out)) {
		case "linux":
			return "linux", nil
		case "darwin":
			return "darwin", nil
		}
	}

	if _, ok := probeWindowsShell(h, "[Environment]::OSVersion.Platform", func(s string) bool {
		return strings.Contains(strings.ToLower(s), "win")
	}); ok {
		return "windows", nil
	}

	if err != nil {
		return "", fmt.Errorf("detecting OS: uname failed: %w", err)
	}
	return "", fmt.Errorf("detecting OS: uname returned %q", out)
}

// DetectArch probes the remote host for its CPU architecture and returns "amd64" or "arm64".
func DetectArch(h *Host) (string, error) {
	out, err := h.Run(`uname -m 2>/dev/null || echo UNKNOWN`)
	if err == nil {
		return normalizeArch(strings.TrimSpace(out))
	}

	// Try PowerShell for Windows. match is `true` for any non-error probe —
	// the original code returns normalizeArch(...) on the first successful
	// probe, even if the value is unparseable (then normalizeArch returns an
	// error). Preserve that exactly: fall back to pwsh only when the
	// primary probe itself errors out, not when its content is bad.
	if arch, ok := probeWindowsShell(h, "$env:PROCESSOR_ARCHITECTURE", func(string) bool { return true }); ok {
		return normalizeArch(arch)
	}

	return "", fmt.Errorf("detecting arch: %w", err)
}

// DetectDockerAvailable checks if Docker is installed and the daemon is reachable on the host.
func DetectDockerAvailable(h *Host) bool {
	var cmd string
	switch h.OS {
	case "windows":
		cmd = `docker info --format "{{.ServerVersion}}" 2>$null`
		out, err := h.RunShell(cmd)
		return err == nil && strings.TrimSpace(out) != ""
	default:
		prefix := ""
		if h.OS == "darwin" {
			prefix = `export PATH="/usr/local/bin:/opt/homebrew/bin:$PATH"; `
		}
		cmd = prefix + `docker info --format '{{.ServerVersion}}' 2>/dev/null`
		out, err := h.Run(cmd)
		return err == nil && strings.TrimSpace(out) != ""
	}
}

func normalizeArch(raw string) (string, error) {
	switch strings.ToLower(raw) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported architecture %q (expected x86_64/amd64 or aarch64/arm64)", raw)
	}
}

package runner

import (
	"fmt"
	"strings"

	"github.com/an-lee/gh-sr/internal/autostart"
	"github.com/an-lee/gh-sr/internal/host"
)

type linuxInstanceProbeResult struct {
	dirExists bool
	svcSh     bool
	kind      autostart.Kind
	// version is the contents of $DIR/.runner-version when the file is
	// readable; empty when the file is absent. Added so the Status hot path
	// can fold the version read into the combined probe and drop the
	// separate nativeRunnerVersion SSH round-trip.
	version string
}

// linuxInstanceProbe reports correlated Linux runner state in one SSH
// round-trip. includeDir adds the directory-existence marker used by orphan
// cleanup; the svc.sh, systemd-unit, and runner-version markers are always
// probed together so the Status path can answer local state + version in a
// single round-trip (same win-class as the svc.sh + autostart fold in PR
// #361 and the orphan-plan probe in PR #358).
//
// Markers emitted by the shell on separate lines:
//
//	D - $DIR exists (orphan cleanup only; includeDir=true)
//	S - $DIR/svc.sh is present
//	U - user-level systemd unit is installed ($HOME/.config/systemd/user/...service)
//	Y - system-level systemd unit is installed (/etc/systemd/system/...service)
//	V - $DIR/.runner-version contents (prefixed; V<contents> on a single line)
//
// The markers are independent: an instance with both svc.sh and a user unit
// emits both S and U. The Go-side parser maps the markers back to the
// (dirExists, svcSh, kind, version) tuple the call sites expect. The V
// marker is additive — callers that don't inspect version ignore the line
// and existing markers behave exactly as before.
func linuxInstanceProbe(h *host.Host, instance string, includeDir bool) (linuxInstanceProbeResult, error) {
	var result linuxInstanceProbeResult

	san, err := autostart.SanitizeInstance(instance)
	if err != nil {
		return result, err
	}
	base := autostart.ServiceBasename(san)
	dir := h.RunnerDir(instance)
	userPath := fmt.Sprintf(`"$HOME/.config/systemd/user/%s.service"`, base)
	sysPath := fmt.Sprintf(`/etc/systemd/system/%s.service`, base)

	dirProbe := ""
	if includeDir {
		dirProbe = fmt.Sprintf(`if [ -d %s ]; then echo D; fi; `, dir)
	}
	// dir contains a literal $HOME prefix. Keep it unquoted so the remote shell
	// expands it; the instance segment was sanitized above. v is captured
	// unconditionally (empty when the file is missing) so we can emit a
	// single V marker line on a separate line from the other markers; the
	// POSIX single-line emit avoids awk/sed in the script.
	script := fmt.Sprintf(
		`%sif [ -f %s/svc.sh ]; then echo S; fi; `+
			`if [ -f %s ]; then echo U; `+
			`elif [ -f %s ]; then echo Y; `+
			`fi; `+
			`v=$(cat %s/.runner-version 2>/dev/null || true); `+
			`if [ -n "$v" ]; then printf 'V%%s\n' "$v"; fi`,
		dirProbe, dir, userPath, sysPath, dir,
	)
	out, err := h.Run(script)
	if err != nil {
		return result, err
	}
	// SplitSeq avoids the upfront []string allocation that strings.Split makes
	// for the entire marker output before iteration. linuxInstanceProbe runs
	// once per Status tick per native Linux instance and once per orphan
	// cleanup pass, so the alloc drop compounds at both rates (see
	// BenchmarkLinuxInstanceProbe).
	for line := range strings.SplitSeq(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "D":
			result.dirExists = true
		case trimmed == "S":
			result.svcSh = true
		case trimmed == "U":
			result.kind = autostart.KindSystemdUser
		case trimmed == "Y":
			result.kind = autostart.KindSystemdSystem
		case strings.HasPrefix(trimmed, "V"):
			// V marker is one or more lines of "V<contents>". Take the
			// marker letter off and trust the rest of the line as the
			// version string. Newlines between V lines collapse to a
			// single space so version-with-spaces remains single-valued;
			// the runner version is conventionally a single word in
			// practice (x.y.z[.N]) so this is purely defensive.
			if result.version != "" {
				result.version += " "
			}
			result.version += strings.TrimPrefix(trimmed, "V")
		}
	}
	return result, nil
}

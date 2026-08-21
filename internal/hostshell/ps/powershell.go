// Package ps centralises local powershell.exe invocation flags so call sites
// in host/, diskschedule/, and elsewhere cannot drift apart (issue #281).
package ps

import (
	"fmt"
	"os/exec"
	"strconv"
)

const exe = "powershell.exe"

var stdFlags = []string{"-NoProfile", "-NonInteractive", "-Command"}

// runCmd is the exec.Command constructor used by Exec and CombinedOutput.
// Tests override it to capture argv without spawning powershell.exe.
var runCmd = exec.Command

// CommandArgs returns argv for exec.Command to run script via powershell.exe.
func CommandArgs(script string) []string {
	args := make([]string, 0, 1+len(stdFlags)+1)
	args = append(args, exe)
	args = append(args, stdFlags...)
	args = append(args, script)
	return args
}

// Exec runs script via powershell.exe and returns stdout.
func Exec(script string) ([]byte, error) {
	args := CommandArgs(script)
	return runCmd(args[0], args[1:]...).Output()
}

// CombinedOutput runs script via powershell.exe and returns combined stdout+stderr.
func CombinedOutput(script string) ([]byte, error) {
	args := CommandArgs(script)
	return runCmd(args[0], args[1:]...).CombinedOutput()
}

// CommandLine builds a full command string suitable for host.Host.Run on Windows.
func CommandLine(script string) string {
	return fmt.Sprintf("%s -NoProfile -NonInteractive -Command %s", exe, strconv.Quote(script))
}

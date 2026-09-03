package config

import (
	"fmt"
	"os"
	"strings"
)

// ApplyEnvFile reads a dotenv-style file and sets os environment variables for each KEY=value line.
// If the file does not exist, it returns nil. Lines starting with # and empty lines are ignored.
func ApplyEnvFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading env file: %w", err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		val = unquoteEnvValue(val)
		if key != "" {
			if err := os.Setenv(key, val); err != nil {
				return fmt.Errorf("setting env %s: %w", key, err)
			}
		}
	}
	return nil
}

func unquoteEnvValue(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

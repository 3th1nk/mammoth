package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// LoadEnvFile applies KEY=VALUE pairs from path to the process environment
// (dotenv semantics). Existing environment variables WIN over file entries —
// explicit env is the stronger declaration (same rule as docker CLI --env-file
// vs -e and Go's own os.Expand conventions). Supported syntax: blank lines,
// `#` comments, optional `export ` prefix, single/double quotes stripped.
// No interpolation, no multi-line values: a mounted config file is deliberately
// a flat key list (12-factor stays the primary surface; this is a loader for
// bare-binary deployments without systemd EnvironmentFile or compose env_file).
func LoadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("config: env file: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("config: env file %s line %d: not KEY=VALUE", path, lineNo)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("config: env file %s line %d: empty key", path, lineNo)
		}
		value = unquote(strings.TrimSpace(value))
		// Existing environment wins — the file seeds, env overrides.
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("config: env file %s line %d: %w", path, lineNo, err)
			}
		}
	}
	return scanner.Err()
}

// unquote strips one layer of matching single or double quotes.
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

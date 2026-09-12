package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadEnvFileSeedsWithoutOverriding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mammoth.env")
	content := `
# comment line
MAMMOTH_DATABASE_URL=postgres://file/db
MAMMOTH_PROBE_WAIT=30m
export MAMMOTH_LOG_FORMAT="text"
BAD LINE WITHOUT EQUALS
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAMMOTH_DATABASE_URL", "postgres://env/db") // env wins over file
	t.Setenv("MAMMOTH_ENV_FILE", path)

	if _, err := Load(); err == nil {
		t.Fatal("want error for malformed line, got nil")
	}

	// Fix the file, retry: seeds apply, env precedence holds.
	if err := os.WriteFile(path, []byte("# comment\nMAMMOTH_DATABASE_URL=postgres://file/db\nMAMMOTH_PROBE_WAIT=30m\nexport MAMMOTH_LOG_FORMAT=\"text\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != "postgres://env/db" {
		t.Fatalf("env must win over file: %q", cfg.DatabaseURL)
	}
	if cfg.ProbeWait != 30*time.Minute {
		t.Fatalf("file seed not applied: %v", cfg.ProbeWait)
	}
	if cfg.LogFormat != "text" {
		t.Fatalf("quoted export line not parsed: %q", cfg.LogFormat)
	}
}

func TestFromEnvFailsFastOnInvalidValues(t *testing.T) {
	t.Setenv("MAMMOTH_DATABASE_URL", "postgres://x/db")
	t.Setenv("MAMMOTH_PROBE_PREFIX", "abc")
	t.Setenv("MAMMOTH_PROBE_WAIT", "soon")
	t.Setenv("MAMMOTH_NFS_EXPORT", "maybe")
	_, err := FromEnv()
	if err == nil {
		t.Fatal("want aggregated error for invalid values")
	}
	for _, want := range []string{"MAMMOTH_PROBE_PREFIX", "MAMMOTH_PROBE_WAIT", "MAMMOTH_NFS_EXPORT"} {
		if !contains(err.Error(), want) {
			t.Errorf("error should mention %s: %v", want, err)
		}
	}
}

func TestFromEnvDefaults(t *testing.T) {
	t.Setenv("MAMMOTH_DATABASE_URL", "postgres://x/db")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProbePrefix != 24 || cfg.LayoutRetention != 10 || cfg.VisibilityTimeout != 30*time.Second {
		t.Fatalf("defaults drifted: %+v", cfg)
	}
}

func TestFromEnvRequiresDatabaseURL(t *testing.T) {
	os.Unsetenv("MAMMOTH_DATABASE_URL")
	if _, err := FromEnv(); err == nil {
		t.Fatal("want error for missing MAMMOTH_DATABASE_URL")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

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
	_, err := FromEnv()
	if err == nil {
		t.Fatal("want aggregated error for invalid values")
	}
	for _, want := range []string{"MAMMOTH_PROBE_PREFIX", "MAMMOTH_PROBE_WAIT"} {
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

func TestPXEModeValidation(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr []string // empty = must succeed
	}{
		{"default is off", nil, nil},
		{"explicit off accepts", map[string]string{"MAMMOTH_PXE_MODE": "off"}, nil},
		{"off rejects pool", map[string]string{"MAMMOTH_PXE_MODE": "off", "MAMMOTH_PXE_DHCP_POOL": "10.0.0.10-10.0.0.20"}, []string{"off owns no PXE face"}},
		{"off rejects next-server", map[string]string{"MAMMOTH_PXE_MODE": "off", "MAMMOTH_PXE_NEXT_SERVER": "10.0.0.1"}, []string{"off owns no PXE face"}},
		{"invalid value", map[string]string{"MAMMOTH_PXE_MODE": "turbo"}, []string{"want builtin|external|off"}},
		{"external rejects pool", map[string]string{"MAMMOTH_PXE_MODE": "external", "MAMMOTH_PXE_DHCP_POOL": "10.0.0.10-10.0.0.20"}, []string{"owns no DHCP"}},
		{"builtin requires next-server", map[string]string{"MAMMOTH_PXE_MODE": "builtin", "MAMMOTH_EXTERNAL_URL": "http://mammoth.example:8080"}, []string{"MAMMOTH_PXE_NEXT_SERVER"}},
		{"builtin derives next-server from an IP-literal ExternalURL", map[string]string{"MAMMOTH_PXE_MODE": "builtin", "MAMMOTH_EXTERNAL_URL": "http://192.168.1.7:8080"}, nil},
		{"pxe strategy requires a live mode", map[string]string{"MAMMOTH_BOOT_STRATEGY": "pxe"}, []string{"MAMMOTH_PXE_MODE=builtin|external"}},
		{"pxe strategy ok with external", map[string]string{"MAMMOTH_BOOT_STRATEGY": "pxe", "MAMMOTH_PXE_MODE": "external"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("MAMMOTH_DATABASE_URL", "postgres://x/db")
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			cfg, err := FromEnv()
			if len(c.wantErr) == 0 {
				if err != nil {
					t.Fatalf("want success, got %v", err)
				}
				if c.env == nil && cfg.PXEMode != "off" {
					t.Fatalf("default PXEMode = %q, want off", cfg.PXEMode)
				}
				if c.env["MAMMOTH_PXE_MODE"] == "builtin" && cfg.PXENextServer != "192.168.1.7" {
					t.Fatalf("derived next-server = %q, want 192.168.1.7", cfg.PXENextServer)
				}
				return
			}
			if err == nil {
				t.Fatal("want error, got nil")
			}
			for _, want := range c.wantErr {
				if !contains(err.Error(), want) {
					t.Errorf("error should mention %q: %v", want, err)
				}
			}
		})
	}
}

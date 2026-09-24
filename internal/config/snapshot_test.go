package config

import (
	"testing"
	"time"
)

// The snapshot is the only place configuration leaves the process: every
// denylisted key must be present in the projection (so the mask loop can
// fire), and none of them may ever carry a real value. A denylist entry
// missing from the snapshot map would silently fall out of the response —
// this test fails long before that ships.
func TestSnapshotRedactedKeysAlwaysPresentAndMasked(t *testing.T) {
	cfg := Config{
		Mode:                      ModeAll,
		DatabaseURL:               "postgres://system:hunter2@127.0.0.1:15432/mammoth_dev",
		APIToken:                  "dev-console-token",
		MasterKey:                 "mFTyjwdarDNJecnW5IgRhBKqWaGSJZwx/njReRFyVfc=",
		PXEEnrollToken:            "enroll-secret",
		WindowsInstallSMBUNC:      `\\192.168.1.10\os_iso`,
		WindowsInstallSMBUser:     "mammoth-smb",
		WindowsInstallSMBPassword: "smb-pass",
		MediaRelayAddr:            "192.168.1.10:22",
		MediaRelayUser:            "relay",
		MediaRelayPassword:        "relay-pass",
		MediaRelayDir:             "/data/export",
	}
	snap := cfg.Snapshot()

	leaked := []string{"dev-console-token", "hunter2", "mFTyjwdarDNJecnW5IgRhBKqWaGSJZwx", "enroll-secret", "smb-pass", "relay-pass"}
	for _, s := range leaked {
		for k, v := range snap {
			if vs, ok := v.(string); ok && vs == s {
				t.Errorf("secret value of %s leaked into snapshot", k)
			}
		}
	}
	for _, key := range RedactedKeys {
		v, ok := snap[key]
		if !ok {
			t.Errorf("denylisted key %s missing from snapshot (mask loop silently skipped)", key)
			continue
		}
		if v != "***" {
			t.Errorf("configured redacted key %s = %v, want \"***\"", key, v)
		}
	}
}

// Unset (empty) redacted keys stay null so the console can present
// configured/unconfigured state; every other key stringifies its effective
// value in the env's own vocabulary.
func TestSnapshotValuesAndUnsetRedacted(t *testing.T) {
	cfg := Config{
		Mode:                 ModeAPI,
		DatabaseURL:          "postgres://x/db",
		HTTPAddr:             ":8080",
		LogLevel:             "info",
		RunnerConcurrency:    10,
		VisibilityTimeout:    30 * time.Second,
		BMCTLSInsecure:       true,
		BiosConfirmRequired:  true,
		EraseConfirmRequired: false,
		VerifyReadyWait:      10 * time.Minute,
		// Everything SMB/relay/enroll left zero → unset redacted keys.
	}
	snap := cfg.Snapshot()

	for _, key := range []string{
		"MAMMOTH_API_TOKEN", "MAMMOTH_MASTER_KEY", "MAMMOTH_PXE_ENROLL_TOKEN",
		"MAMMOTH_WINDOWS_INSTALL_SMB_UNC", "MAMMOTH_MEDIA_RELAY_ADDR",
	} {
		if v := snap[key]; v != nil {
			t.Errorf("unset redacted key %s = %v, want null", key, v)
		}
	}
	if v := snap["MAMMOTH_DATABASE_URL"]; v != "***" {
		t.Errorf("required DSN is always configured, got %v", v)
	}
	checks := map[string]string{
		"MAMMOTH_MODE":               "api",
		"MAMMOTH_HTTP_ADDR":          ":8080",
		"MAMMOTH_RUNNER_CONCURRENCY": "10",
		"MAMMOTH_VISIBILITY_TIMEOUT": "30s",
		"MAMMOTH_VERIFY_READY_WAIT":  "10m0s",
		"MAMMOTH_BMC_TLS_INSECURE":   "true",
		"MAMMOTH_BIOS_CONFIRM":       "required",
		"MAMMOTH_ERASE_CONFIRM":      "optional",
		"MAMMOTH_PXE_ENABLED":        "false",
		"MAMMOTH_NFS_EXPORT":         "false",
	}
	for key, want := range checks {
		got, ok := snap[key].(string)
		if !ok {
			t.Errorf("key %s = %v (%T), want string", key, snap[key], snap[key])
			continue
		}
		if got != want {
			t.Errorf("key %s = %q, want %q", key, got, want)
		}
	}
	// An unset ordinary key reads as null too (IPMIInterface is empty here —
	// the real default is applied in FromEnv, the snapshot is a projection).
	if v := snap["MAMMOTH_IPMI_INTERFACE"]; v != nil {
		t.Errorf("empty ordinary key = %v, want null", v)
	}
}

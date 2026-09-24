package config

import (
	"strconv"
	"time"
)

// RedactedKeys is the denylist of environment keys whose values never leave
// the process through the config snapshot (docs/04 §A6): anything that
// authenticates (tokens, master key, the DSN), names an account (share and
// relay usernames), or pins internal infrastructure (share UNC, relay
// endpoint and export dir) belongs here. Over-redaction costs an operator a
// hint; under-redaction leaks — the list errs on the masking side.
var RedactedKeys = []string{
	"MAMMOTH_API_TOKEN",
	"MAMMOTH_MASTER_KEY",
	"MAMMOTH_DATABASE_URL",
	"MAMMOTH_PXE_ENROLL_TOKEN",
	"MAMMOTH_WINDOWS_INSTALL_SMB_UNC",
	"MAMMOTH_WINDOWS_INSTALL_SMB_USER",
	"MAMMOTH_WINDOWS_INSTALL_SMB_PASSWORD",
	"MAMMOTH_MEDIA_RELAY_ADDR",
	"MAMMOTH_MEDIA_RELAY_USER",
	"MAMMOTH_MEDIA_RELAY_PASSWORD",
	"MAMMOTH_MEDIA_RELAY_DIR",
}

// Snapshot projects the effective configuration as an env-keyed map — the
// 12-factor fact source exactly as the deployment sees it. Values are
// stringified (durations render in Go duration notation, ints and bools in
// their canonical form, MAMMOTH_BIOS_CONFIRM/ERASE_CONFIRM back in their env
// vocabulary); an empty effective value reads as null. Keys in RedactedKeys
// never carry their value: configured ones render as "***", unset ones stay
// null — configured/unconfigured state is visible, values are not. The
// projection is read-only by design: configuration is deployment-owned and
// there is deliberately no write path through the API.
func (c Config) Snapshot() map[string]any {
	m := map[string]any{}
	str := func(k, v string) {
		if v == "" {
			m[k] = nil
		} else {
			m[k] = v
		}
	}
	num := func(k string, v int) { m[k] = strconv.Itoa(v) }
	bol := func(k string, v bool) { m[k] = strconv.FormatBool(v) }
	dur := func(k string, v time.Duration) { m[k] = v.String() }

	str("MAMMOTH_MODE", string(c.Mode))
	str("MAMMOTH_DATABASE_URL", c.DatabaseURL)
	str("MAMMOTH_HTTP_ADDR", c.HTTPAddr)
	str("MAMMOTH_API_TOKEN", c.APIToken)
	str("MAMMOTH_MASTER_KEY", c.MasterKey)
	str("MAMMOTH_LOG_LEVEL", c.LogLevel)
	str("MAMMOTH_LOG_FORMAT", c.LogFormat)
	num("MAMMOTH_RUNNER_CONCURRENCY", c.RunnerConcurrency)
	dur("MAMMOTH_HEARTBEAT_INTERVAL", c.HeartbeatInterval)
	dur("MAMMOTH_VISIBILITY_TIMEOUT", c.VisibilityTimeout)
	dur("MAMMOTH_HEARTBEAT_TIMEOUT", c.HeartbeatTimeout)
	dur("MAMMOTH_REAPER_INTERVAL", c.ReaperInterval)
	num("MAMMOTH_QUEUE_MAX_RECEIVE_COUNT", c.QueueMaxReceiveCount)
	dur("MAMMOTH_QUEUE_RETRY_BACKOFF", c.QueueRetryBackoff)
	dur("MAMMOTH_QUEUE_POLL_INTERVAL", c.QueuePollInterval)
	num("MAMMOTH_RUNNER_MAX_TASK_ATTEMPTS", c.RunnerMaxTaskAttempts)
	dur("MAMMOTH_BMC_TIMEOUT", c.BMCTimeout)
	bol("MAMMOTH_BMC_TLS_INSECURE", c.BMCTLSInsecure)
	str("MAMMOTH_IPMI_INTERFACE", c.IPMIInterface)
	dur("MAMMOTH_IDEMPOTENCY_TTL", c.IdempotencyTTL)
	dur("MAMMOTH_TASK_STATUS_INTERVAL", c.TaskStatusInterval)
	dur("MAMMOTH_TASK_LOGS_TTL", c.TaskLogsTTL)
	num("MAMMOTH_LAYOUT_RETENTION", c.LayoutRetention)
	dur("MAMMOTH_INBAND_TIMEOUT", c.InbandTimeout)
	bol("MAMMOTH_RAMDISK_ENABLED", c.RamdiskEnabled)
	str("MAMMOTH_PROBE_ALPINE_ISO", c.ProbeAlpineISO)
	str("MAMMOTH_PROBE_ALPINE_NETBOOT", c.ProbeAlpineNetboot)
	str("MAMMOTH_PXE_DI_NETBOOT", c.PXEDINetboot)
	str("MAMMOTH_WINDOWS_APPLY_ALPINE_ISO", c.WindowsApplyAlpineISO)
	str("MAMMOTH_PXE_DI_UDEBS_DIR", c.PXEDIUdebsDir)
	str("MAMMOTH_PROBE_STATIC_CIDR", c.ProbeStaticCIDR)
	num("MAMMOTH_PROBE_PREFIX", c.ProbePrefix)
	str("MAMMOTH_PROBE_GATEWAY", c.ProbeGateway)
	dur("MAMMOTH_PROBE_WAIT", c.ProbeWait)
	str("MAMMOTH_MEDIA_DIR", c.MediaDir)
	str("MAMMOTH_MEDIA_WORKDIR", c.MediaWorkDir)
	str("MAMMOTH_MEDIA_BASE_URI", c.MediaBaseURI)
	str("MAMMOTH_WINDOWS_INSTALL_SMB_UNC", c.WindowsInstallSMBUNC)
	str("MAMMOTH_WINDOWS_INSTALL_SMB_USER", c.WindowsInstallSMBUser)
	str("MAMMOTH_WINDOWS_INSTALL_SMB_PASSWORD", c.WindowsInstallSMBPassword)
	dur("MAMMOTH_BOOT_SETTLE_DELAY", c.BootSettleDelay)
	dur("MAMMOTH_VERIFY_READY_WAIT", c.VerifyReadyWait)
	bol("MAMMOTH_NFS_EXPORT", c.NFSExportEnabled)
	num("MAMMOTH_NFS_EXPORT_PORT", c.NFSExportPort)
	str("MAMMOTH_MEDIA_RELAY_ADDR", c.MediaRelayAddr)
	str("MAMMOTH_MEDIA_RELAY_USER", c.MediaRelayUser)
	str("MAMMOTH_MEDIA_RELAY_PASSWORD", c.MediaRelayPassword)
	str("MAMMOTH_MEDIA_RELAY_DIR", c.MediaRelayDir)
	str("MAMMOTH_COMPAT_DIR", c.CompatDir)
	// The confirm gates store a bool; render it back in the env's own
	// vocabulary so the snapshot reads like the deployment's env file.
	if c.BiosConfirmRequired {
		m["MAMMOTH_BIOS_CONFIRM"] = "required"
	} else {
		m["MAMMOTH_BIOS_CONFIRM"] = "optional"
	}
	if c.EraseConfirmRequired {
		m["MAMMOTH_ERASE_CONFIRM"] = "required"
	} else {
		m["MAMMOTH_ERASE_CONFIRM"] = "optional"
	}
	dur("MAMMOTH_FAKE_BMC_DELAY", c.DevFakeBMCDelay)
	str("MAMMOTH_OTEL_EXPORTER_ENDPOINT", c.OTELExporterEndpoint)
	str("MAMMOTH_METRICS_ADDR", c.MetricsAddr)
	str("MAMMOTH_EXTERNAL_URL", c.ExternalURL)
	bol("MAMMOTH_PXE_ENABLED", c.PXEEnabled)
	str("MAMMOTH_PXE_NEXT_SERVER", c.PXENextServer)
	str("MAMMOTH_PXE_MODE", c.PXEMode)
	num("MAMMOTH_PXE_DHCP_PORT", c.PXEDHCPPort)
	num("MAMMOTH_PXE_TFTP_PORT", c.PXETFTPPort)
	num("MAMMOTH_PXE_PROXY_PORT", c.PXEProxyPort)
	num("MAMMOTH_PXE_SYSLOG_PORT", c.PXESyslogPort)
	str("MAMMOTH_PXE_DHCP_POOL", c.PXEDHCPPool)
	str("MAMMOTH_PXE_DHCP_ROUTER", c.PXEDHCPRouter)
	str("MAMMOTH_PXE_BROADCAST_ADDR", c.PXEBroadcastAddr)
	bol("MAMMOTH_PXE_ENROLL", c.PXEEnroll)
	str("MAMMOTH_PXE_ENROLL_TOKEN", c.PXEEnrollToken)
	str("MAMMOTH_BOOT_STRATEGY", c.BootStrategyDefault)

	for _, k := range RedactedKeys {
		if v, ok := m[k]; ok && v != nil {
			m[k] = "***"
		}
	}
	return m
}

//go:build probe_dev

// Dev-only harness for the ramdisk probe ISO (docs/compat/huawei.md ramdisk
// retrospective): builds the probe ISO from a local alpine ISO for the local
// qemu iteration loop. Not part of the regular test suite — run with:
//
//	go test -tags probe_dev ./internal/builder -run TestDevProbeISO
//
// Environment: MAMMOTH_PROBE_DEV_ISO (alpine standard ISO path, required),
// MAMMOTH_PROBE_DEV_URL (report URL, default the qemu slirp host),
// MAMMOTH_PROBE_DEV_OUT (output path, default /tmp/probe-dev.iso).

package builder

import (
	"context"
	"os"
	"testing"
)

func TestDevProbeISO(t *testing.T) {
	iso := os.Getenv("MAMMOTH_PROBE_DEV_ISO")
	if iso == "" {
		t.Skip("MAMMOTH_PROBE_DEV_ISO not set; skipping probe ISO dev build")
	}
	out := os.Getenv("MAMMOTH_PROBE_DEV_OUT")
	if out == "" {
		out = "/tmp/probe-dev.iso"
	}
	url := os.Getenv("MAMMOTH_PROBE_DEV_URL")
	if url == "" {
		url = "http://10.0.2.2:8765/render/devtoken/probe-report"
	}
	got, err := BuildProbeISO(context.Background(), ProbeOptions{
		ISOPath:    iso,
		OutputPath: out,
		ReportURL:  url,
	})
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("probe ISO built: %s (%d bytes)", got, fi.Size())
}

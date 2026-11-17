package builder

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
)

func TestCpioRoundTrip(t *testing.T) {
	entries := []cpioEntry{
		{Name: "bin/", Mode: 0o040755},
		{Name: "bin/busybox", Mode: 0o100755, Body: []byte("fake-busybox-binary")},
		{Name: "lib/ld-musl-x86_64.so.1", Mode: 0o100755, Body: []byte("fake-musl-loader")},
		{Name: "sbin/init", Mode: 0o120777, Link: "/bin/busybox"},
		{Name: "etc/init.d/rcS", Mode: 0o100755, Body: []byte("#!/bin/sh\necho probe\n")},
	}
	packed := cpioWriteNewc(entries)
	back, err := cpioReadNewc(packed)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(back) != len(entries) {
		t.Fatalf("entry count = %d, want %d", len(back), len(entries))
	}
	for i, e := range back {
		w := entries[i]
		if e.Name != w.Name {
			t.Errorf("entry %d name = %q, want %q", i, e.Name, w.Name)
		}
		if string(e.Body) != string(w.Body) {
			t.Errorf("entry %q body = %q, want %q", e.Name, e.Body, w.Body)
		}
		if e.Link != w.Link {
			t.Errorf("entry %q link = %q, want %q", e.Name, e.Link, w.Link)
		}
		if e.Mode != w.Mode {
			t.Errorf("entry %q mode = %o, want %o", e.Name, e.Mode, w.Mode)
		}
	}
}

func TestMiniprootSegment(t *testing.T) {
	carrier := cpioWriteNewc([]cpioEntry{
		{Name: "init", Mode: 0o100755, Body: []byte("alpine init script")},
		{Name: "bin/", Mode: 0o040755},
		{Name: "bin/busybox", Mode: 0o100755, Body: []byte("carrier-busybox")},
		{Name: "lib/", Mode: 0o040755},
		{Name: "lib/ld-musl-x86_64.so.1", Mode: 0o100755, Body: []byte("carrier-musl-loader")},
	})
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	gw.Write(carrier)
	gw.Close()

	seg, err := miniprootSegment(gz.Bytes(), nil,
		"http://10.0.0.1:8080/netboot/enroll/tok", "lts", false, 514)
	if err != nil {
		t.Fatalf("miniprootSegment: %v", err)
	}
	zr, zerr := gzip.NewReader(bytes.NewReader(seg))
	if zerr != nil {
		t.Fatalf("segment is not gzip: %v", zerr)
	}
	plain, rerr := io.ReadAll(zr)
	if rerr != nil {
		t.Fatalf("segment gunzip: %v", rerr)
	}
	entries, err := cpioReadNewc(plain)
	if err != nil {
		t.Fatalf("segment parse: %v", err)
	}
	have := map[string]cpioEntry{}
	for _, e := range entries {
		have[e.Name] = e
	}
	for _, name := range []string{"init", "bin/busybox", "lib/ld-musl-x86_64.so.1", "bin/sh"} {
		if _, ok := have[name]; !ok {
			t.Errorf("segment missing %s", name)
		}
	}
	if string(have["bin/busybox"].Body) != "carrier-busybox" {
		t.Error("busybox body must come from the carrier initramfs")
	}
	if have["bin/sh"].Link != "busybox" {
		t.Errorf("bin/sh link = %q", have["bin/sh"].Link)
	}
	init := string(have["init"].Body)
	if !bytes.Contains([]byte(init), []byte("http://10.0.0.1:8080/netboot/enroll/tok")) {
		t.Error("/init must carry the baked report URL")
	}
	if bytes.Contains([]byte(init), []byte("udhcpc -i")) && !bytes.Contains([]byte(init), []byte("-s /tmp/udhcpc.sh")) {
		t.Error("udhcpc must use the inline script, not missing helpers")
	}
	if !strings.Contains(init, "[miniproot]") {
		t.Error("/init must carry [miniproot] log lines for console diagnosis")
	}
	if !strings.Contains(init, "logger -h 10.0.0.1 -P 514 -t miniproot") {
		t.Error("syslog sink must target the report URL host — the engine this boot serves")
	}
	if bytes.Contains([]byte(init), []byte("172.16.20")) {
		t.Error("/init must never carry a hardcoded foreign-engine address")
	}
}

// TestMiniprootInitSyslogFollowsReportURL: the syslog sink derives from the
// report URL (the engine this boot serves) — a hardcoded address sent
// probe milestones from every test segment to one foreign engine's sink.
func TestMiniprootInitSyslogFollowsReportURL(t *testing.T) {
	hostless := miniprootInitScript("", "lts", false, 0)
	if strings.Contains(hostless, "logger -h") {
		t.Error("hostless report URL must not render a logger target")
	}
	if !strings.Contains(hostless, `echo "[miniproot] $*"`) {
		t.Error("console-only LOGD must keep the [miniproot] echo")
	}
	if got := miniprootInitScript("http://192.168.1.132:8080/render/t/probe-report", "lts", false, 5514); !strings.Contains(got, "logger -h 192.168.1.132 -P 5514 -t miniproot") {
		t.Error("syslog target must follow the report URL host and the configured sink port")
	}
	if got := miniprootInitScript("http://192.168.1.132:8080/render/t/probe-report", "lts", false, 0); !strings.Contains(got, "logger -h 192.168.1.132 -P 514 -t miniproot") {
		t.Error("zero sink port must default to 514")
	}
}

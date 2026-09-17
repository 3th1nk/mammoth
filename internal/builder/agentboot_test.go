package builder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
)

func TestAgentOverlay(t *testing.T) {
	name, data, err := AgentOverlay()
	if err != nil {
		t.Fatalf("AgentOverlay: %v", err)
	}
	if name != "mammoth.apkovl.tar.gz" {
		t.Errorf("overlay name = %q", name)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("overlay is not gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	seen := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		body, _ := io.ReadAll(tr)
		seen[hdr.Name] = string(body)
	}
	script, ok := seen["etc/local.d/mammoth-agent.start"]
	if !ok {
		t.Fatalf("overlay missing the agent script (entries: %v)", keysOf(seen))
	}
	for _, want := range []string{
		"agent-plan.sh",
		"mammoth_disk",
		"mammoth_partition",
		"sfdisk --force",
		"apk add --root \"$TARGET\"",
		"grub-install --root-directory=\"$TARGET\" --removable --no-nvram",
		"MAMMOTH_COMPLETE_URL",
		"reboot -f",
		"C12A7328-F81F-11D2-BA4B-00A0C93EC93B", // EFI System Partition GUID
	} {
		if !strings.Contains(script, want) {
			t.Errorf("agent script missing %q", want)
		}
	}
	if _, ok := seen["etc/runlevels/default/local"]; !ok {
		t.Errorf("overlay missing the openrc local runlevel symlink")
	}
	if _, ok := seen["etc/.default_boot_services"]; !ok {
		t.Errorf("overlay missing the default boot services marker")
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

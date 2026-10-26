package mediarelay

import (
	"fmt"
	"strings"
	"testing"
)

// The push command streams stdin into the TEMP name and publishes by rename.
// Regression: the first shape wrote the final name directly and then mv-ed a
// never-created temp onto it — every push transferred fully and died at mv
// (exit 1), unseen because the relay had no test.
func TestPushCommand(t *testing.T) {
	cmd := pushCommand("/srv/mammoth-media", "boot/boot-abc.iso")
	for _, want := range []string{
		`mkdir -p "/srv/mammoth-media/boot"`,
		`cat > "/srv/mammoth-media/.boot-abc.iso.part"`,
		`mv "/srv/mammoth-media/.boot-abc.iso.part" "/srv/mammoth-media/boot/boot-abc.iso"`,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("push command missing %q:\n%s", want, cmd)
		}
	}
	// cat must write the temp, not the final — the mv source must exist.
	if strings.Contains(cmd, fmt.Sprintf("cat > %q", "/srv/mammoth-media/boot/boot-abc.iso")) {
		t.Errorf("push command must stream into the temp name:\n%s", cmd)
	}
}

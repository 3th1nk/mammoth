package builder

// The agent runtime's dd payload (roadmap「镜像直写通路」slice 1): the
// dispatch branches on MAMMOTH_DD_MODE before the declarative install chain,
// and dd_image streams wget → gunzip → tee(disk) → sha256sum — single pass,
// checksum verified on the bytes that hit the disk (pipefail makes every
// upstream failure fatal).

import (
	"strings"
	"testing"
)

func TestRuntimeDDDispatchAndStream(t *testing.T) {
	script := agentScript()
	if !strings.Contains(script, `if [ "$MAMMOTH_DD_MODE" = "1" ]; then`) {
		t.Errorf("runtime has no MAMMOTH_DD_MODE dispatch branch")
	}
	// The branch must precede the declarative install chain (bootstrap_tools)
	// — dd machines have no pool to bootstrap from.
	if strings.Index(script, `if [ "$MAMMOTH_DD_MODE" = "1" ]; then`) > strings.Index(script, "bootstrap_tools || bail tools") {
		t.Errorf("dd dispatch must run before bootstrap_tools")
	}
	for _, want := range []string{
		`dd_image || bail dd`,
		`wget -q -O - "$MAMMOTH_DD_URL" | $decomp | tee "$MAMMOTH_DD_TARGET" | sha256sum`,
		"set -o pipefail",
		`raw.gz) decomp="gunzip -c" ;;`,
		`reboot -f`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("runtime missing %q", want)
		}
	}
}

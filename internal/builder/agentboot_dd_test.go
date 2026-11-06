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
		`seed_partition || bail seed`,
		`echo "$MAMMOTH_SEED_USER_DATA" | base64 -d > /tmp/seedmnt/user-data`,
		"$MKFS_V -n CIDATA",
		`dd_image || bail dd`,
		// conv=fsync write + READ-BACK verification: the stream hash cannot
		// see what the disk holds (the 2288H lesson — a "verified" tee write
		// read back as no-partition-table on LSI write-back cache). The
		// partition tools are absolute util-linux/dosfstools binaries: the
		// PATH-first busybox sfdisk does not speak GPT (same lesson).
		`dd of="$MAMMOTH_DD_TARGET" bs=1M conv=fsync`,
		"blockdev --flushbufs",
		`dd if="$MAMMOTH_DD_TARGET" bs=1M count=$blocks 2>/dev/null | head -c "$MAMMOTH_DD_IMAGE_BYTES" | sha256sum`,
		"read-back sha mismatch",
		"set -o pipefail",
		`raw.gz) decomp="gunzip -c" ;;`,
		`raw.xz) decomp="xz -dc" ;;`,
		"apk add --no-cache xz",
		
		`reboot -f`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("runtime missing %q", want)
		}
	}
}

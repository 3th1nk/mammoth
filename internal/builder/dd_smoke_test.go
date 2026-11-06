package builder

// The dd pathway's container smoke (roadmap「镜像直写通路」slice ③ closure,
// transport layer): the runtime's dd_image runs inside an alpine container —
// busybox wget streaming over HTTP, tee-to-target, sha256sum of the written
// bytes, gzip CRC and the pipeline-fail path are the behaviors unit
// assertions cannot cover. The seed partition surgery (sfdisk/CIDATA) is
// NOT asserted here: alpine's util-linux sfdisk refuses ordinary files
// (libfdisk EINVAL, deterministic — not a probability) and container
// loop-device node allocation is a Docker artifact — that layer is covered
// by the qemu full-chain round, whose kernel/block-device semantics match
// production. Two more Docker-on-macOS facts the smoke baked in: the inner
// script must live under /tmp (inside Docker Desktop's file-sharing scope —
// t.TempDir() is not, and the mount silently reads empty), and the HTTP
// source is busybox-extras' httpd (the default busybox carries no httpd
// applet). Opt-in (needs docker):
// MAMMOTH_DD_SMOKE=1 go test -run TestDDImageSmoke.

import (
	"os"
	"os/exec"
	"testing"
)

func TestDDImageSmoke(t *testing.T) {
	if os.Getenv("MAMMOTH_DD_SMOKE") != "1" {
		t.Skip("MAMMOTH_DD_SMOKE=1 to run the dd container smoke")
	}
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	inner := `
log() { echo "[runtime] $*"; }
bail() { echo "[bail] $*"; exit 1; }
report() { echo "[report] $*"; return 0; }
` +
		extractShFunc(t, agentScript(), "dd_image") + `
# ── smoke driver ──
set -e
dd if=/dev/urandom of=/work/disk.img bs=1M count=12 2>/dev/null
sha256sum /work/disk.img | awk '{print $1}' > /work/src.sha
gzip -kf /work/disk.img
dd if=/dev/zero of=/work/target.img bs=1M count=48 2>/dev/null
httpd -p 8080 -h /work &
sleep 1
export MAMMOTH_DD_URL=http://127.0.0.1:8080/disk.img.gz
export MAMMOTH_DD_SHA256=$(cat /work/src.sha)
export MAMMOTH_DD_FORMAT=raw.gz
export MAMMOTH_DD_TARGET=/work/target.img
dd_image
# the written bytes hash to the source image
got=$(sha256sum /work/disk.img | awk '{print $1}')
[ "$got" = "$MAMMOTH_DD_SHA256" ] || { echo "TARGET HASH UNVERIFIABLE"; exit 1; }
echo SMOKE_OK
`
	if err := os.WriteFile("/tmp/dd-inner.sh", []byte(inner), 0o755); err != nil {
		t.Fatal(err)
	}
	docker := exec.Command("docker", "run", "--rm", "--privileged",
		"-v", "/tmp:/hosttmp",
		"alpine:3.20",
		"sh", "-c", "apk add -q busybox-extras >/dev/null 2>&1; mkdir -p /work; cp /hosttmp/dd-inner.sh /work/inner.sh; sh /work/inner.sh; rc=$?; echo INNER_RC=$rc; exit $rc")
	docker.Stdout = os.Stdout
	docker.Stderr = os.Stderr
	if err := docker.Run(); err != nil {
		t.Fatalf("dd smoke container failed: %v", err)
	}
}

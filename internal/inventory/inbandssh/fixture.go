package inbandssh

import (
	"context"
	"strings"
	"time"
)

// StaticRunner serves a canned output for the in-band command set. It backs
// fake machines (fake:// addresses): the partition-level pipeline — snapshot
// capture, keep/preserve binding, drift baselines — is exercised end to end
// without real hosts, against the same recorded output shape a real machine
// produces.
type StaticRunner struct {
	// Output is the combined script output.
	Output []byte
	// Delay simulates collection latency (optional).
	Delay time.Duration
	// FailWhen, when set, makes every Run fail with this error.
	FailWhen error
}

func (r *StaticRunner) Run(_ context.Context, _ string, _ Credentials, _ string) ([]byte, error) {
	if r.Delay > 0 {
		time.Sleep(r.Delay)
	}
	if r.FailWhen != nil {
		return nil, r.FailWhen
	}
	return r.Output, nil
}

// SwitchRunner selects the transport by address scheme: fake:// machines talk
// to the StaticRunner; everything else goes over SSH.
type SwitchRunner struct {
	SSH   Runner
	Fake  Runner
	Delay time.Duration // fake-side latency (heartbeat/reaper exercise)
}

func (r *SwitchRunner) Run(ctx context.Context, addr string, cred Credentials, script string) ([]byte, error) {
	if strings.HasPrefix(addr, "fake://") {
		if r.Delay > 0 {
			time.Sleep(r.Delay)
		}
		return r.Fake.Run(ctx, addr, cred, script)
	}
	return r.SSH.Run(ctx, addr, cred, script)
}

// DefaultFixture is a two-disk machine's recorded output: NVMe (GPT, EFI +
// root) and SATA (dos, /data), matching the shape of the golden test fixture
// in the collector tests.
const DefaultFixture = `{
  "blockdevices": [
    {
      "name": "nvme0n1", "path": "/dev/nvme0n1", "size": 500107862016, "type": "disk",
      "serial": "S6XPN0001", "pttype": "GPT",
      "children": [
        {"name": "nvme0n1p1", "path": "/dev/nvme0n1p1", "size": 536870912, "type": "part",
         "fstype": "vfat", "partlabel": "EFI", "partuuid": "1111-2222", "mountpoint": "/boot/efi"},
        {"name": "nvme0n1p2", "path": "/dev/nvme0n1p2", "size": 499569602560, "type": "part",
         "fstype": "xfs", "mountpoint": "/"}
      ]
    },
    {
      "name": "sda", "path": "/dev/sda", "size": 1000204886016, "type": "disk",
      "serial": "GIM256_0001", "pttype": "dos",
      "children": [
        {"name": "sda1", "path": "/dev/sda1", "size": 536870912, "type": "part",
         "fstype": "ext4", "mountpoint": "/data"}
      ]
    },
    {"name": "loop0", "path": "/dev/loop0", "size": 4096, "type": "loop"}
  ]
}
===BLKID===
/dev/nvme0n1p1: UUID="F8A1-9C03" TYPE="vfat" PARTUUID="1111-2222" PARTLABEL="EFI"
/dev/nvme0n1p2: UUID="9f86d081-884c-7d65-9a2f-aaa63c07a73b" TYPE="xfs"
/dev/sda1: UUID="b2a1c3d4-0000-1111-2222-333344445555" TYPE="ext4"
===LINK===
[{"ifindex":1,"ifname":"lo","flags":["LOOPBACK","UP"],"mtu":65536,"operstate":"UNKNOWN","address":"00:00:00:00:00:00"},
 {"ifindex":2,"ifname":"eno1","flags":["BROADCAST","MULTICAST","UP"],"mtu":1500,"operstate":"UP","address":"aa:bb:cc:dd:ee:01"},
 {"ifindex":3,"ifname":"eno2","flags":["BROADCAST","MULTICAST"],"mtu":1500,"operstate":"DOWN","address":"aa:bb:cc:dd:ee:02"}]
===PARTS===
/sys/block/nvme0n1/nvme0n1p1/start 2048
/sys/block/nvme0n1/nvme0n1p2/start 1050624
/sys/block/sda/sda1/start 2048
`

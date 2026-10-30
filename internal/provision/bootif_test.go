package provision

import "testing"

// TestBootifFor pins the per-entry BOOTIF rewrite: the BMC's one-time PXE
// picks whichever LOM it pleases, so every netboot entry must carry args
// naming ITS OWN NIC — a shared args with one MAC's BOOTIF hangs casper's
// ipconfig on the port that was not chosen.
func TestBootifFor(t *testing.T) {
	mac := "50:1d:93:d8:c6:97"
	want := "BOOTIF=01-50-1d-93-d8-c6-97"

	t.Run("rewrites mid-token bootif", func(t *testing.T) {
		args := "autoinstall ds=nocloud-net;s=http://x/ ip=dhcp console=tty0 BOOTIF=01-50-1d-93-d8-c6-98 boot=casper"
		got := bootifFor(args, mac)
		wantArgs := "autoinstall ds=nocloud-net;s=http://x/ ip=dhcp console=tty0 " + want + " boot=casper"
		if got != wantArgs {
			t.Fatalf("got  %s\nwant %s", got, wantArgs)
		}
	})

	t.Run("appends when absent", func(t *testing.T) {
		got := bootifFor("autoinstall ds=nocloud-net;s=http://x/", mac)
		if got != "autoinstall ds=nocloud-net;s=http://x/ "+want {
			t.Fatalf("got %s", got)
		}
	})

	t.Run("bootif at end of args", func(t *testing.T) {
		got := bootifFor("autoinstall BOOTIF=01-50-1d-93-d8-c6-98", mac)
		if got != "autoinstall "+want {
			t.Fatalf("got %s", got)
		}
	})

	t.Run("empty args", func(t *testing.T) {
		if got := bootifFor("", mac); got != want {
			t.Fatalf("got %s", got)
		}
	})

	t.Run("normalizes dot-form mac", func(t *testing.T) {
		if got := bootifFor("", "50.1d.93.d8.c6.97"); got != want {
			t.Fatalf("got %s", got)
		}
	})
}

package netboot

import (
	"context"
	"encoding/binary"
	"fmt"
	"io/fs"
	"net"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func discardUDPLog(string, ...any) {}

// startTestTFTP serves files on an ephemeral loopback port.
func startTestTFTP(t *testing.T, files fs.FS) *net.UDPAddr {
	t.Helper()
	loopback := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
	conn, err := net.ListenUDP("udp", loopback)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); conn.Close() })
	go serveTFTP(ctx, conn, files, discardUDPLog)
	return conn.LocalAddr().(*net.UDPAddr)
}

// rrqClient is a minimal TFTP client exercising exactly what PXE ROMs and
// iPXE do: RRQ to the well-known port, then the lockstep ACK loop against
// the SESSION port (the source of each received datagram — RFC 1350's
// transfer-id rule).
type rrqClient struct {
	conn    *net.UDPConn
	blksize int
	peer    *net.UDPAddr // session endpoint learned from the first reply
}

func startClient(t *testing.T) *rrqClient {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &rrqClient{conn: c, blksize: tftpBlockSize}
}

func (c *rrqClient) send(to *net.UDPAddr, pkt []byte) {
	if _, err := c.conn.WriteToUDP(pkt, to); err != nil {
		panic(err)
	}
}

func (c *rrqClient) recv() []byte {
	c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	b := make([]byte, 65536)
	n, from, err := c.conn.ReadFromUDP(b)
	if err != nil {
		panic(err)
	}
	c.peer = from // replies and ACKs must track the session port
	return b[:n]
}

func (c *rrqClient) fetch(to *net.UDPAddr, name string, opts ...string) []byte {
	rrq := append([]byte{0, tftpRRQ}, (name + "\x00octet\x00")...)
	for i := 0; i+1 < len(opts); i += 2 {
		rrq = append(rrq, (opts[i] + "\x00" + opts[i+1] + "\x00")...)
	}
	c.send(to, rrq)

	first := c.recv()
	if binary.BigEndian.Uint16(first[:2]) == tftpOACK {
		// Acknowledge the OACK with block 0, then data follows at block 1.
		c.send(c.peer, []byte{0, tftpACK, 0, 0})
		first = c.recv()
	}
	if op := binary.BigEndian.Uint16(first[:2]); op != tftpDATA {
		panic(fmt.Sprintf("want DATA, got op=%d", op))
	}
	wantBlock := binary.BigEndian.Uint16(first[2:4])
	var out []byte
	for {
		out = append(out, first[4:]...)
		ack := make([]byte, 4)
		binary.BigEndian.PutUint16(ack, tftpACK)
		binary.BigEndian.PutUint16(ack[2:], wantBlock)
		c.send(c.peer, ack)
		if len(first)-4 < c.blksize {
			return out // short block = last
		}
		next := c.recv()
		if op := binary.BigEndian.Uint16(next[:2]); op != tftpDATA {
			panic("unexpected non-DATA")
		}
		wantBlock++
		first = next
	}
}

func TestTFTPRoundTrip(t *testing.T) {
	files := fstest.MapFS{
		"undionly.kpxe":  &fstest.MapFile{Data: bytesN(2048)},
		"ipxe-amd64.efi": &fstest.MapFile{Data: bytesN(1)},
	}
	addr := startTestTFTP(t, files)

	t.Run("classic 512-byte blocks", func(t *testing.T) {
		got := startClient(t).fetch(addr, "undionly.kpxe")
		if len(got) != 2048 {
			t.Fatalf("got %d bytes, want 2048", len(got))
		}
	})

	t.Run("blksize and tsize negotiation", func(t *testing.T) {
		c := startClient(t)
		c.blksize = 1428
		// Probe tsize via OACK: read the OACK packet ourselves.
		rrq := []byte{0, tftpRRQ}
		rrq = append(rrq, "undionly.kpxe\x00octet\x00tsize\x000\x00blksize\x001428\x00"...)
		c.send(addr, rrq)
		oack := c.recv()
		if binary.BigEndian.Uint16(oack[:2]) != tftpOACK {
			t.Fatalf("want OACK, got op=%d", binary.BigEndian.Uint16(oack[:2]))
		}
		if !strings.Contains(string(oack), "tsize\x002048") {
			t.Errorf("oack missing tsize: %q", oack[4:])
		}
		if !strings.Contains(string(oack), "blksize\x001428") {
			t.Errorf("oack missing blksize: %q", oack[4:])
		}
		c.send(c.peer, []byte{0, tftpACK, 0, 0})
		// Full fetch through the negotiated block size.
		var out []byte
		block := uint16(0)
		for {
			pkt := c.recv()
			if binary.BigEndian.Uint16(pkt[:2]) != tftpDATA {
				t.Fatalf("want DATA, got op=%d", binary.BigEndian.Uint16(pkt[:2]))
			}
			if blk := binary.BigEndian.Uint16(pkt[2:4]); blk != block+1 {
				t.Fatalf("block %d, want %d", blk, block+1)
			}
			block++
			out = append(out, pkt[4:]...)
			ack := make([]byte, 4)
			binary.BigEndian.PutUint16(ack, tftpACK)
			binary.BigEndian.PutUint16(ack[2:], block)
			c.send(c.peer, ack)
			if len(pkt)-4 < c.blksize {
				break
			}
		}
		if len(out) != 2048 {
			t.Fatalf("transferred %d bytes, want 2048", len(out))
		}
	})

	t.Run("unknown file stays silent", func(t *testing.T) {
		c := startClient(t)
		c.send(addr, append([]byte{0, tftpRRQ}, ("nope.bin\x00octet\x00")...))
		c.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		b := make([]byte, 512)
		if _, _, err := c.conn.ReadFromUDP(b); err == nil {
			t.Fatal("unknown file must not produce a datagram")
		}
	})
}

func TestParseRRQ(t *testing.T) {
	pkt := append([]byte{0, tftpRRQ}, "f\x00octet\x00blksize\x001024\x00tsize\x00\x00"...)
	name, opts, err := parseRRQ(pkt)
	if err != nil || name != "f" || !opts.tsize || opts.blksize != 1024 {
		t.Fatalf("parseRRQ = %q %+v %v", name, opts, err)
	}
	// Oversized blksize caps at the no-fragmentation limit.
	_, opts, _ = parseRRQ(append([]byte{0, tftpRRQ}, "f\x00octet\x00blksize\x0099999\x00"...))
	if opts.blksize != tftpMaxBlkSize {
		t.Fatalf("blksize = %d, want cap %d", opts.blksize, tftpMaxBlkSize)
	}
	if _, _, err := parseRRQ([]byte{0, tftpACK, 0, 1}); err == nil {
		t.Fatal("ACK must not parse as RRQ")
	}
}

func bytesN(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

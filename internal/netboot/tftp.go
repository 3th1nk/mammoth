package netboot

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"strconv"
	"strings"
	"time"
)

// TFTP wire constants (RFC 1350 + RFC 2348/2349 options).
const (
	tftpRRQ  = 1
	tftpDATA = 3
	tftpACK  = 4
	tftpOACK = 6

	tftpTimeout    = 1 * time.Second
	tftpMaxRetries = 5
	// OACK acknowledgements get fewer retries before tolerance kicks in:
	// some PXE ROMs (Huawei X722 NIC shim, 2288H) never ACK the OACK at
	// all and expect DATA to just follow — each wasted second here is a
	// second of the boot chain, and the client's own RRQ retry makes the
	// old give-up path self-heal only after ~10s.
	tftpOACKRetries = 2
	// Classic 512 vs option-negotiated blocks; the cap keeps replies inside
	// one Ethernet frame so they never fragment (fragmented UDP + TFTP is a
	// known firmware trap).
	tftpBlockSize  = 512
	tftpMaxBlkSize = 1428
)

// serveTFTP answers read requests from files (the embedded NBP set) until
// ctx ends. One datagram socket is shared for RRQ reception; each transfer
// moves to its own ephemeral socket — the client locks onto the source port
// of the first reply, so sessions never interleave.
func serveTFTP(ctx context.Context, conn *net.UDPConn, files fs.FS, render func(string, net.IP) []byte, log func(format string, args ...any)) {
	// Session sockets bind the same local address as the listener — on a
	// multi-addressed host an unbound socket can pick a source the client's
	// RPF drops, and it also pins the source family (no v4-vs-[::] drift).
	local := conn.LocalAddr().(*net.UDPAddr).IP
	buf := make([]byte, 4*1024)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		name, opts, perr := parseRRQ(buf[:n])
		if perr != nil {
			continue // garbage: drop, the client retries
		}
		// grubnet requests paths rooted at its (tftp)/grub/ prefix with a
		// leading slash; the embedded FS is slash-less.
		name = strings.TrimPrefix(name, "/")
		c, cerr := net.ListenUDP("udp", &net.UDPAddr{IP: local})
		if cerr != nil {
			continue
		}
		go func() {
			defer c.Close()
			transferTFTP(c, addr, files, render, name, opts, log)
		}()
	}
}

// rrqOptions is what the client asked for beyond a classic read.
type rrqOptions struct {
	blksize int
	tsize   bool
}

// parseRRQ decodes "opcode\0filename\0mode\0[opt\0value\0]...".
func parseRRQ(b []byte) (string, rrqOptions, error) {
	opts := rrqOptions{blksize: tftpBlockSize}
	if len(b) < 2 || binary.BigEndian.Uint16(b[:2]) != tftpRRQ {
		return "", opts, errors.New("not an rrq")
	}
	parts := strings.Split(string(b[2:]), "\x00")
	if len(parts) < 3 || parts[0] == "" {
		return "", opts, errors.New("short rrq")
	}
	name, mode := parts[0], parts[1]
	if !strings.EqualFold(mode, "octet") {
		return "", opts, fmt.Errorf("unsupported mode %q", mode)
	}
	for i := 2; i+1 < len(parts); i += 2 {
		switch strings.ToLower(parts[i]) {
		case "blksize":
			// RFC 2348: an oversized request is not an error — the OACK
			// names the (smaller) value actually used, so clamp.
			if n, err := strconv.Atoi(parts[i+1]); err == nil && n > 0 {
				opts.blksize = max(8, min(n, tftpMaxBlkSize))
			}
		case "tsize":
			opts.tsize = true
		}
	}
	return name, opts, nil
}

// transferTFTP streams one file to one client: OACK when options were
// requested, then lockstep DATA/ACK with retransmit on timeout. A name the
// render hook answers (the per-MAC grub.cfg) is served from memory; anything
// else comes from the static embedded FS.
func transferTFTP(c *net.UDPConn, addr *net.UDPAddr, files fs.FS, render func(string, net.IP) []byte, name string, opts rrqOptions, log func(string, ...any)) {
	var r io.Reader
	var size int64
	var knowsSize bool
	var closer io.Closer

	if render != nil {
		if content := render(name, addr.IP); content != nil {
			r = bytes.NewReader(content)
			size = int64(len(content))
			knowsSize = true
		}
	}
	if r == nil {
		f, err := files.Open(name)
		if err != nil {
			log("tftp: no such file %q (client %s)", name, addr.IP)
			// RFC 1350 §5: answer with an ERROR datagram. Clients probing
			// optional files (shim's revocations_*/shim_certificate_*
			// Secure-Boot fetches) treat a proper "file not found" as absent
			// and move on; silence makes the EFI PXE client block in a
			// download-retry loop (real-hardware: shim stalled 4s/req forever).
			sendError(c, addr, 1, "File not found")
			return
		}
		closer = f
		if rs, ok := f.(io.ReadSeeker); ok {
			if s, serr := rs.Seek(0, io.SeekEnd); serr == nil {
				size, knowsSize = s, true
				if _, err := rs.Seek(0, io.SeekStart); err != nil {
					knowsSize = false
				}
			}
		}
		r = f
	}
	defer func() {
		if closer != nil {
			closer.Close()
		}
	}()

	if opts.tsize || opts.blksize != tftpBlockSize {
		var oack []byte
		oack = binary.BigEndian.AppendUint16(oack, tftpOACK)
		oack = binary.BigEndian.AppendUint16(oack, 0)
		if opts.tsize && knowsSize {
			oack = append(oack, "tsize\x00"...)
			oack = append(oack, strconv.FormatInt(size, 10)+"\x00"...)
		}
		if opts.blksize != tftpBlockSize {
			oack = append(oack, "blksize\x00"...)
			oack = append(oack, strconv.Itoa(opts.blksize)+"\x00"...)
		}
		// Tolerance over strictness: a client that ACKs the OACK proceeds
		// normally (block-0 ACK received, lockstep starts at DATA 1). A
		// client that stays silent on the OACK — the X722 shim, which never
		// ACKs block 0 — is served the first DATA block anyway: it either
		// accepts it and the transfer proceeds, or keeps stalling and
		// sendFile's own retries end the session. Abandoning the transfer
		// here instead only forced the client to re-RRQ (its ~10s self-heal
		// on real hardware).
		if !sendExpectACKN(c, addr, oack, 0, tftpOACKRetries) {
			log("tftp: client %s did not ack oack for %q — sending data anyway", addr.IP, name)
		}
	}
	if err := sendFile(c, addr, r, opts.blksize); err != nil {
		log("tftp: transfer of %q to %s failed: %v", name, addr.IP, err)
	}
}

// sendFile runs the lockstep block loop (RFC 1350 §8: one DATA, wait ACK,
// retransmit the same block on timeout). A short or empty final block is
// the EOF signal — when the file size is an exact multiple of blksize the
// terminating zero-length block must still be sent, or the client waits
// forever.
func sendFile(c *net.UDPConn, addr *net.UDPAddr, f io.Reader, blksize int) error {
	block := uint16(0)
	buf := make([]byte, blksize)
	for {
		n, _ := f.Read(buf)
		block++ // wraps deliberately: TFTP block numbers are mod 2^16
		pkt := make([]byte, 4+n)
		binary.BigEndian.PutUint16(pkt, tftpDATA)
		binary.BigEndian.PutUint16(pkt[2:], block)
		copy(pkt[4:], buf[:n])
		if !sendExpectACK(c, addr, pkt, block) {
			return fmt.Errorf("client stalled at block %d", block)
		}
		if n < blksize {
			return nil
		}
	}
}

// sendExpectACK sends pkt and waits for the ACK of block want, resending up
// to tftpMaxRetries times.
func sendExpectACK(c *net.UDPConn, addr *net.UDPAddr, pkt []byte, want uint16) bool {
	return sendExpectACKN(c, addr, pkt, want, tftpMaxRetries)
}

// sendExpectACKN is sendExpectACK with the retry budget spelled out — the
// OACK wait uses fewer retries before the X722 tolerance takes over.
func sendExpectACKN(c *net.UDPConn, addr *net.UDPAddr, pkt []byte, want uint16, retries int) bool {
	for range retries {
		if _, err := c.WriteToUDP(pkt, addr); err != nil {
			return false
		}
		c.SetReadDeadline(time.Now().Add(tftpTimeout))
		if got, ok := expectACK(c); ok && got == want {
			return true
		}
	}
	return false
}

// sendError answers with a TFTP ERROR datagram (opcode 5): code 1 = file not
// found. The message is NUL-terminated per RFC 1350 §5.
func sendError(c *net.UDPConn, addr *net.UDPAddr, code uint16, msg string) {
	pkt := make([]byte, 4+len(msg)+1)
	binary.BigEndian.PutUint16(pkt, 5)
	binary.BigEndian.PutUint16(pkt[2:], code)
	copy(pkt[4:], msg)
	c.WriteToUDP(pkt, addr)
}

// expectACK waits for the next ACK datagram and returns its block number.
func expectACK(c *net.UDPConn) (uint16, bool) {
	var b [4]byte
	for {
		n, err := c.Read(b[:])
		if err != nil {
			return 0, false
		}
		if n < 4 || binary.BigEndian.Uint16(b[:2]) != tftpACK {
			continue
		}
		return binary.BigEndian.Uint16(b[2:]), true
	}
}

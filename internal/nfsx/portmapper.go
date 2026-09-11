// Minimal rpcbind/portmapper (RFC 1050 v2) for the built-in NFS export.
//
// BMC virtual-media clients resolve the mount and NFS service ports through
// the portmapper before mounting (observed on iBMC 6.41: the mount dance
// opens rpcbind/111 first — docs/compat/huawei.md). go-nfs multiplexes nfs
// and mountd on ONE port, so the mapper answers every lookup for the two
// programs with that port. UDP and TCP are both served; TCP uses the RPC
// record-marking protocol.
package nfsx

import (
	"context"
	"encoding/binary"
	"net"
	"time"
)

const (
	pmapProgram   = 100000
	pmapVersion   = 2
	nfsProgram    = 100003
	mountProgram  = 100005
	rpcVersion    = 2
	replyAccepted = 1
	msgCall       = 0
	msgReply      = 1
	acceptSuccess = 0
	acceptProcNA  = 2
	protoTCP      = 6
	protoUDP      = 17
)

type portmapper struct {
	port int // the single port nfs and mountd are served on
}

// call is the decoded header of an RPC CALL.
type rpcCall struct {
	xid     uint32
	prog    uint32
	vers    uint32
	proc    uint32
	restOff int // offset of the program-specific args in the payload
}

// decode parses the fixed prefix of a CALL record.
func decodeCall(p []byte) (rpcCall, bool) {
	if len(p) < 40 {
		return rpcCall{}, false
	}
	if binary.BigEndian.Uint32(p[4:8]) != msgCall ||
		binary.BigEndian.Uint32(p[8:12]) != rpcVersion {
		return rpcCall{}, false
	}
	credFlavor := binary.BigEndian.Uint32(p[12:16])
	credLen := binary.BigEndian.Uint32(p[16:20])
	off := 20 + int(credLen)
	if credFlavor != 0 { // skip non-null cred body
		off = 16 + int(credLen)
		if len(p) < off+4 {
			return rpcCall{}, false
		}
		off += 4 + int(binary.BigEndian.Uint32(p[off:off+4])) // verf len handled below
	}
	// verf: flavor(4) + len(4) + body
	if len(p) < off+8 {
		return rpcCall{}, false
	}
	verfLen := binary.BigEndian.Uint32(p[off+4 : off+8])
	off += 8 + int(verfLen)
	if len(p) < off+16 {
		return rpcCall{}, false
	}
	return rpcCall{
		xid:     binary.BigEndian.Uint32(p[0:4]),
		prog:    binary.BigEndian.Uint32(p[12:16]),
		vers:    binary.BigEndian.Uint32(p[16:20]),
		proc:    binary.BigEndian.Uint32(p[24:28]),
		restOff: off + 12, // prog/vers/proc consumed from the creds tail
	}, true
}

// servePortmap answers rpcbind lookups until ctx ends.
func (pm *portmapper) serve(ctx context.Context, port int) error {
	udpAddr := &net.UDPAddr{Port: port}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return err
	}
	tcpListener, err := net.ListenTCP("tcp", &net.TCPAddr{Port: port})
	if err != nil {
		udpConn.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		udpConn.Close()
		tcpListener.Close()
	}()
	go pm.serveUDP(ctx, udpConn)
	go pm.serveTCP(ctx, tcpListener)
	return nil
}

func (pm *portmapper) serveUDP(ctx context.Context, conn *net.UDPConn) {
	buf := make([]byte, 1500)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if reply, ok := pm.dispatch(buf[:n]); ok {
			_, _ = conn.WriteToUDP(reply, addr)
		}
	}
}

func (pm *portmapper) serveTCP(ctx context.Context, ln *net.TCPListener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(30 * time.Second))
			for {
				// RPC over TCP: 4-byte record marker (high bit = last fragment).
				var mark [4]byte
				if _, err := ioReadFull(c, mark[:]); err != nil {
					return
				}
				size := int(binary.BigEndian.Uint32(mark[:]) & 0x7fffffff)
				if size == 0 || size > 64*1024 {
					return
				}
				payload := make([]byte, size)
				if _, err := ioReadFull(c, payload); err != nil {
					return
				}
				reply, ok := pm.dispatch(payload)
				if !ok {
					return
				}
				marker := uint32(len(reply)) | 0x80000000
				var head [4]byte
				binary.BigEndian.PutUint32(head[:], marker)
				if _, err := c.Write(head[:]); err != nil {
					return
				}
				if _, err := c.Write(reply); err != nil {
					return
				}
			}
		}(conn)
	}
}

// dispatch answers one RPC CALL record.
func (pm *portmapper) dispatch(payload []byte) ([]byte, bool) {
	call, ok := decodeCall(payload)
	if !ok {
		return nil, false
	}
	if call.prog != pmapProgram || call.vers != pmapVersion {
		return nil, false // let the client time out / retry elsewhere
	}
	switch call.proc {
	case 0: // NULL — empty accepted reply
		return acceptedReply(call.xid, acceptSuccess, u32(0)), true
	case 3: // GETPORT: args prog, vers, proto, port
		if len(payload) < call.restOff+16 {
			return acceptedReply(call.xid, acceptSuccess, u32(0)), true
		}
		prog := binary.BigEndian.Uint32(payload[call.restOff : call.restOff+4])
		_ = protoTCP // documented: both protocols resolve to the same port
		_ = protoUDP
		var port uint32
		if prog == nfsProgram || prog == mountProgram {
			port = uint32(pm.port)
		}
		return acceptedReply(call.xid, acceptSuccess, u32(port)), true
	case 1, 2: // SET/UNSET — accept silently
		return acceptedReply(call.xid, acceptSuccess, u32(1)), true
	case 4: // DUMP — empty list
		return acceptedReply(call.xid, acceptSuccess, nil), true
	default:
		return acceptedReply(call.xid, acceptProcNA, nil), true
	}
}

// acceptedReply builds an ACCEPTED reply with the given accept state and body.
func acceptedReply(xid uint32, state uint32, body []byte) []byte {
	out := make([]byte, 0, 28+len(body))
	out = appendU32(out, xid)
	out = appendU32(out, msgReply)
	out = appendU32(out, 0)     // reply_stat: MSG_ACCEPTED
	out = appendU32(out, 0)     // verf flavor: AUTH_NULL
	out = appendU32(out, 0)     // verf length
	out = appendU32(out, state) // accept_stat
	out = append(out, body...)
	return out
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// ioReadFull reads exactly len(buf) bytes.
func ioReadFull(c net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := c.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

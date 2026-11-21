//go:build !linux

// Raw receive channel is Linux-only (AF_PACKET). On other platforms it
// degrades unconditionally — checksum-broken PXE clients were observed on
// Linux hosts and the UDP path keeps serving well-behaved ones.

package netboot

import (
	"context"
	"fmt"
	"net"
)

func (s *Server) startRawRecv(ctx context.Context, dhcpConn, proxyConn, tftpConn *net.UDPConn) error {
	return fmt.Errorf("raw recv: not available on this platform")
}

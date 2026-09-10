// Package nfsx embeds a read-only NFSv3 export of the media directory into
// mammoth (the built-in media service). Out of the box the BMC mounts
// nfs://<mammoth-host>/<export> directly from the process that builds the
// media — no external NFS server, no relay, no upload path.
//
// The implementation is the user-space go-nfs server: this is a deliberate
// trade — kernel nfsd outperforms it and is the reference for BMC
// compatibility, but requires kernel modules and privileged setup. The
// built-in export targets evaluation and small/mid fleets; large-scale
// deployments should point MAMMOTH_MEDIA_BASE_URI at an external NFS
// service (MAMMOTH_NFS_EXPORT=false) — docs/09-roadmap.md M6.
//
// KNOWN LIMITS (verify before relying at scale):
//   - go-nfs implements NFSv3 + the mount protocol multiplexed on one
//     port; it does NOT provide a portmapper (rpcbind on 111). Clients
//     that look up the mount port via rpcbind need verification — the
//     iBMC 6.41 client was observed querying rpcbind during its mount
//     dance; the real-hardware check is pending.
//   - Writes through NFS land in an in-memory overlay (memphis) and never
//     touch the export directory on disk.
package nfsx

import (
	"context"
	"fmt"
	"net"

	"github.com/willscott/memphis"
	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
)

// Server is one running export.
type Server struct {
	listener net.Listener
	done     chan error
}

// Start exports dir read-only over NFSv3 on port (mount + nfs multiplexed
// on the same listener). Blocks briefly to bind; serving continues in the
// background until ctx ends.
func Start(ctx context.Context, dir string, port int) (*Server, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("nfsx: bind :%d: %w", port, err)
	}
	// The OS directory is served through an in-memory view: NFS writes are
	// accepted by the protocol but never reach the disk — the export is
	// effectively read-only at the storage layer.
	fs := memphis.FromOS(dir).AsBillyFS(0, 0)
	handler := nfshelper.NewCachingHandler(nfshelper.NewNullAuthHandler(fs), 1024)

	s := &Server{listener: listener, done: make(chan error, 1)}
	go func() { s.done <- nfs.Serve(listener, handler) }()
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-s.done:
		}
	}()
	return s, nil
}

// Wait reports how the server ended (error, or nil on clean shutdown).
func (s *Server) Wait() error { return <-s.done }

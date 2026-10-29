// Package nfsx embeds a read-only NFSv3 export of the media directory into
// mammoth. Opt-in (MAMMOTH_NFS_EXPORT=true): the supported media export is a
// deployment-layer kernel nfsd — docs/operations.md §4.7 — nfsx remains for
// unprivileged evaluation and clients that mount the EXPORT ROOT.
//
// The implementation is the user-space go-nfs server: pure TCP, NFSv3 +
// mount protocol multiplexed on one port, AUTH_NULL only, and every mount
// path answered with the export root (path resolution is left to the
// caller); rpcbind lookups are served by the in-package minimal portmapper
// (portmapper.go — UDP+TCP on 111, GETPORT answers 2049 for both nfs and
// mountd).
//
// CONSUMER MATRIX (why this is not the default):
//   - BMC virtual media: Huawei iBMC 6.41 completes GETPORT and NULL-pings
//     2049 OK, then closes without MNT — Connect fails against nfsx while
//     the same URI mounts from a kernel nfsd (real hardware, huawei.md §6).
//   - casper nfsroot (ubuntu22 PXE) and anaconda NFS sources mount SUBPATHS
//     of the export; the root-answering MNT resolves those lookups against
//     the wrong tree, so a path-resolving server (kernel nfsd) is required.
//   - Clients that mount the export root and iterate it work: manual
//     inspection, and fake-BMC evaluation where nothing actually mounts.
//
// Writes through NFS land in an in-memory overlay (memphis) and never touch
// the export directory on disk.
package nfsx

import (
	"context"
	"fmt"
	"net"

	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
	"github.com/willscott/memphis"
)

// Server is one running export.
type Server struct {
	listener net.Listener
	done     chan error
	pm       *portmapper
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
	// rpcbind/portmapper on 111: BMC mount clients resolve the mount/nfs
	// ports here before mounting. go-nfs multiplexes both on one port, so
	// every lookup for nfs(100003)/mountd(100005) answers with it.
	pm := &portmapper{port: port}
	if perr := pm.serve(ctx, 111); perr != nil {
		listener.Close()
		return nil, fmt.Errorf("nfsx: portmapper: %w", perr)
	}
	s.pm = pm
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

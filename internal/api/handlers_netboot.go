package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/netboot"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/store"
)

// ── machine-facing netboot surface (docs/06-install-pipeline.md §3.3) ──────
// iPXE runs inside the target machine and cannot hold a bearer token; the
// task token (boot tree files) and the entry registry (script endpoint)
// carry the trust. These endpoints are public by contract (security: []),
// mirroring /render/{token}/....

// GetNetbootScript answers iPXE's per-MAC script fetch. No pending entry is
// NOT an error: the fallback script exits back to the firmware boot order,
// which lands the machine on its local disk (the post-install race and the
// stray-machine case share this path).
func (s *Server) GetNetbootScript(ctx context.Context, request gen.GetNetbootScriptRequestObject) (gen.GetNetbootScriptResponseObject, error) {
	mac := netboot.NormalizeMAC(request.Params.Mac)
	if mac == "" {
		return nil, verrStatus(http.StatusBadRequest, "SCHEMA_INVALID_MAC",
			"mac parameter %q is not a hardware address", request.Params.Mac)
	}
	e, err := s.Netboot.Entry(ctx, mac)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return gen.GetNetbootScript200TextResponse(netboot.NoEntryScript(mac)), nil
	}
	s.Events.Append(ctx, "task", e.TaskID, "task.netboot_script_served", map[string]any{
		"mac": mac, "kind": e.Kind,
	})
	obs.FromContext(ctx).InfoContext(ctx, "netboot script served",
		obs.FieldTaskID, e.TaskID, "mac", mac, "kind", e.Kind)
	return gen.GetNetbootScript200TextResponse(netboot.RenderScript(e, s.ExternalURL)), nil
}

// FetchNetbootFile serves the per-task boot tree (kernel/initrd/aux files).
// Only names registered on the entry are servable — flat names, no path
// separators, so traversal is structurally impossible.
func (s *Server) FetchNetbootFile(ctx context.Context, request gen.FetchNetbootFileRequestObject) (gen.FetchNetbootFileResponseObject, error) {
	e, err := s.NetbootRepo.ByToken(ctx, request.Token)
	if err != nil {
		return nil, err
	}
	if !entryServes(e, request.File) {
		return nil, verrStatus(http.StatusNotFound, "RENDER_UNKNOWN_FILE",
			"file %q is not part of this boot tree", request.File)
	}
	path := filepath.Join(s.MediaDir, "netboot", e.Token, request.File)
	f, err := os.Open(path)
	if err != nil {
		return nil, verrStatus(http.StatusNotFound, "RENDER_UNKNOWN_FILE",
			"file %q is not available yet", request.File)
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, verrStatus(http.StatusNotFound, "RENDER_UNKNOWN_FILE",
			"file %q is not available", request.File)
	}
	// File ownership transfers to the generated response visitor: it is
	// read (and closed, being an io.ReadCloser) only after this handler
	// returns — deferring Close here truncated every transfer with
	// "file already closed" (2288H real-hardware finding).
	return gen.FetchNetbootFile200ApplicationoctetStreamResponse{
		Body:          f,
		ContentLength: st.Size(),
	}, nil
}

// entryServes is the file allowlist: the entry's kernel, initrd, and any
// auxiliary files registered in extra (e.g. the alpine modloop).
func entryServes(e *store.NetbootEntry, name string) bool {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return false
	}
	for _, n := range e.AllowlistedFiles() {
		if n == name {
			return true
		}
	}
	return false
}

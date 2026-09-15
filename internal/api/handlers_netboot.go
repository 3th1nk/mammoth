package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/netboot"
	"github.com/3th1nk/mammoth/internal/obs"
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
		// Zero-registration entry (docs/09-roadmap.md): when enrollment is
		// on, an unknown MAC is offered the shared probe payload and reports
		// itself into pending_machines; otherwise fall through to disk.
		if s.Enroll != nil {
			// pending resource dimension — the MAC is not a machine id yet.
			s.Events.Append(ctx, "pending", mac, "pending.enroll_served", map[string]any{
				"mac": mac,
			})
			obs.FromContext(ctx).InfoContext(ctx, "enroll script served", "mac", mac)
			return gen.GetNetbootScript200TextResponse(netboot.RenderEnrollScript(s.Enroll.Tree, s.ExternalURL, mac)), nil
		}
		return gen.GetNetbootScript200TextResponse(netboot.NoEntryScript(mac)), nil
	}
	s.Events.Append(ctx, "task", e.TaskID, "task.netboot_script_served", map[string]any{
		"mac": mac, "kind": e.Kind,
	})
	obs.FromContext(ctx).InfoContext(ctx, "netboot script served",
		obs.FieldTaskID, e.TaskID, "mac", mac, "kind", e.Kind)
	return gen.GetNetbootScript200TextResponse(netboot.RenderScript(e, s.ExternalURL)), nil
}

// FetchNetbootFile serves the per-task boot tree. Two grant shapes live in
// the entry: flat file names (kernel/initrd) and subtree grants
// (Extra value "dir:apks" → everything under tree/apks/, for the probe's
// apk repository). Paths are cleaned and confined to the granted subtree —
// traversal is structurally impossible.
func (s *Server) FetchNetbootFile(ctx context.Context, request gen.FetchNetbootFileRequestObject) (gen.FetchNetbootFileResponseObject, error) {
	e, err := s.NetbootRepo.ByToken(ctx, request.Token)
	if err != nil {
		return nil, err
	}
	rel, ok := entryGrants(e, request.File)
	if !ok {
		return nil, verrStatus(http.StatusNotFound, "RENDER_UNKNOWN_FILE",
			"file %q is not part of this boot tree", request.File)
	}
	path := filepath.Join(s.MediaDir, "netboot", e.Token, rel)
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

// entryGrants resolves a requested file to its on-disk path relative to the
// boot tree: flat allowlisted names map directly; "dir:<name>" extra grants
// map anything under that subtree. ok=false for anything ungranted or that
// tries to escape (.., absolute, separators above the granted depth). Both
// the store row and the machine-face projection satisfy the interface.
func entryGrants(e interface {
	AllowlistedFiles() []string
	GrantedSubtrees() []string
}, name string) (string, bool) {
	if name != "" && name != "." && name != ".." && filepath.Base(name) == name {
		for _, n := range e.AllowlistedFiles() {
			if n == name {
				return name, true
			}
		}
	}
	if strings.Contains(name, "..") {
		return "", false
	}
	clean := filepath.Clean("/" + name) // strips leading ../ chains
	rel := strings.TrimPrefix(clean, "/")
	for _, g := range e.GrantedSubtrees() {
		if rel == g || strings.HasPrefix(rel, g+"/") {
			return rel, true
		}
	}
	return "", false
}

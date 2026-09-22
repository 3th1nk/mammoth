package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

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

// serveGrubByMAC answers the external-PXE trampoline's configfile fetch
// (grub expands ${net_default_mac} into the path). Mirrors the builtin
// TFTP render hook: pending entry → per-MAC config; none → the exit
// fallback that hands control back to the firmware boot order.
func (s *Server) serveGrubByMAC(c *gin.Context) {
	ctx := c.Request.Context()
	mac := netboot.NormalizeMAC(c.Param("mac"))
	if mac == "" {
		c.Data(http.StatusOK, "text/plain; charset=utf-8",
			[]byte(netboot.NoEntryGRUB(c.Param("mac"))))
		return
	}
	e, err := s.Netboot.Entry(ctx, mac)
	if err != nil {
		writeError(c, err)
		return
	}
	if e == nil {
		c.Data(http.StatusOK, "text/plain; charset=utf-8",
			[]byte(netboot.NoEntryGRUB(mac)))
		return
	}
	s.Events.Append(ctx, "task", e.TaskID, "task.netboot_grub_served", map[string]any{
		"mac": mac, "kind": e.Kind,
	})
	obs.FromContext(ctx).InfoContext(ctx, "netboot grub config served",
		obs.FieldTaskID, e.TaskID, "mac", mac, "kind", e.Kind)
	c.Data(http.StatusOK, "text/plain; charset=utf-8",
		[]byte(netboot.RenderGRUB(e, s.ExternalURL)))
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
		obs.FromContext(ctx).InfoContext(ctx, "netboot file rejected", "token", e.Token, "file", request.File)
		return nil, verrStatus(http.StatusNotFound, "RENDER_UNKNOWN_FILE",
			"file %q is not part of this boot tree", request.File)
	}
	path := filepath.Join(s.MediaDir, "netboot", e.Token, rel)
	f, err := os.Open(path)
	if err != nil {
		obs.FromContext(ctx).InfoContext(ctx, "netboot file miss", "token", e.Token, "file", rel)
		return nil, verrStatus(http.StatusNotFound, "RENDER_UNKNOWN_FILE",
			"file %q is not available yet", request.File)
	}
	obs.FromContext(ctx).InfoContext(ctx, "netboot file served", "file", rel)
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

// poolStoreSHARe is the content address a shared pool tree is keyed by —
// anything else in the :sha slot is a probe, not a tree.
var poolStoreSHARe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// fetchPoolStoreFile serves the shared, content-addressed pool tree
// (MediaDir/pool-store/<sha256>/iso/... and, for the windows agent
// apply-image pathway, .../win/tree/...): one unpack per image content,
// consumed by d-i (HTTP mirror), casper (http fallback) across tasks, and
// the windows agent (install.wim + the ESP boot files). The path is
// confined to the addressed tree — the sha slot is format-checked and the
// rest cannot traverse (.. rejected, Clean strips chains). The windows
// subtree is the same trust domain as the ISO: prepared-media content
// (the injected SetupComplete pair is generic, nothing per-task), served
// like the ISO itself.
func (s *Server) fetchPoolStoreFile(c *gin.Context, sha, rest string) error {
	if !poolStoreSHARe.MatchString(sha) || strings.Contains(rest, "..") {
		return verrStatus(http.StatusNotFound, "RENDER_UNKNOWN_FILE",
			"no pool tree at this address")
	}
	rel := strings.TrimPrefix(filepath.Clean("/"+rest), "/")
	// The first path segment names the subtree ("iso" = the distro ISO
	// unpack; "win" = the prepared windows tree) — it is also the on-disk
	// directory below pool-store/<sha>/, so strip it once: joining it back
	// doubled the segment and 404'd every fetch.
	sub, inner, ok := strings.Cut(rel, "/")
	if !ok || (sub != "iso" && sub != "win") {
		return verrStatus(http.StatusNotFound, "RENDER_UNKNOWN_FILE",
			"no pool tree at this address")
	}
	path := filepath.Join(s.MediaDir, "pool-store", sha, sub, inner)
	f, err := os.Open(path)
	if err != nil {
		return verrStatus(http.StatusNotFound, "RENDER_UNKNOWN_FILE",
			"no pool tree at this address")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return verrStatus(http.StatusNotFound, "RENDER_UNKNOWN_FILE",
			"no pool tree at this address")
	}
	obs.FromContext(c.Request.Context()).InfoContext(c.Request.Context(),
		"pool store file served", "sha256", sha, "file", rel)
	c.DataFromReader(http.StatusOK, st.Size(), "application/octet-stream", f, nil)
	return nil
}

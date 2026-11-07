package api

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

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
	// The script fetch IS the machine talking from its boot address: on
	// site-DHCP shapes (external engine, proxy DHCP, or a DHCP-only spec)
	// this is the only moment the engine learns which address the machine
	// boots from — the one fact the syslog sink's IP attribution needs (the
	// lease-reverse chain exists only on the builtin pool). TTL is generous:
	// the terminal release forgets the task's registrations earlier, and a
	// retried task re-fetches and re-registers anyway.
	if s.IPRegistry != nil && e.TaskID != "" {
		if ip := ClientIPFromContext(ctx); ip != "" {
			s.IPRegistry.Register(e.TaskID, []string{ip}, 12*time.Hour)
		}
	}
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
	f, st, err := s.openBootTreeFile(ctx, request.Token, request.File)
	if err != nil {
		return nil, err
	}
	// Range fetches answer straight on the gin writer and return a nil
	// response: the strict contract has no 206 shape, and the generated
	// wrapper writes nothing when response and error are both nil. Without
	// this, a dropped connection mid-initrd (hundreds of MB over the
	// provisioning LAN) restarts the fetch from byte zero — resumable
	// clients (curl/wget, d-i retriever, casper) only pick up at the last
	// byte when the server honors Range.
	if gc := rangeFetch(ctx); gc != nil {
		defer f.Close()
		serveRangeAware(gc, f, st)
		return nil, nil
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

// openBootTreeFile resolves a boot-tree file request to an open handle:
// entry lookup by token, grant check (flat allowlist + subtree grants),
// then open + regular-file stat under MediaDir/netboot/<token>/. Shared by
// the strict handler's whole-body path and its Range branch.
func (s *Server) openBootTreeFile(ctx context.Context, token, file string) (*os.File, os.FileInfo, error) {
	e, err := s.NetbootRepo.ByToken(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	rel, ok := entryGrants(e, file)
	if !ok {
		obs.FromContext(ctx).InfoContext(ctx, "netboot file rejected", "token", e.Token, "file", file)
		return nil, nil, verrStatus(http.StatusNotFound, "RENDER_UNKNOWN_FILE",
			"file %q is not part of this boot tree", file)
	}
	path := filepath.Join(s.MediaDir, "netboot", e.Token, rel)
	f, err := os.Open(path)
	if err != nil {
		obs.FromContext(ctx).InfoContext(ctx, "netboot file miss", "token", e.Token, "file", rel)
		return nil, nil, verrStatus(http.StatusNotFound, "RENDER_UNKNOWN_FILE",
			"file %q is not available yet", file)
	}
	obs.FromContext(ctx).InfoContext(ctx, "netboot file served", "file", rel)
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, nil, verrStatus(http.StatusNotFound, "RENDER_UNKNOWN_FILE",
			"file %q is not available", file)
	}
	return f, st, nil
}

// rangeFetch returns the gin context when the request carries a Range
// header. The generated strict wrapper passes the *gin.Context AS the
// handler's ctx (server.go ContextWithFallback note) — that is the only
// channel through which a strict handler can see request headers or reach
// the writer, and the only place a 206 can be produced without touching
// generated code.
func rangeFetch(ctx context.Context) *gin.Context {
	gc, ok := ctx.(*gin.Context)
	if !ok || gc.GetHeader("Range") == "" {
		return nil
	}
	return gc
}

// serveRangeAware hands f to http.ServeContent: 206 + Content-Range for
// resumable clients, 416 for unsatisfiable ranges, 304 for conditional
// fetches, and Accept-Ranges on every answer so the client learns resume is
// an option. Content-Type is preset to keep the octet-stream the contract
// declares (ServeContent otherwise sniffs it from the file extension).
func serveRangeAware(c *gin.Context, f *os.File, st os.FileInfo) {
	c.Header("Content-Type", "application/octet-stream")
	http.ServeContent(c.Writer, c.Request, st.Name(), st.ModTime(), f)
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
	// ServeContent instead of a bare stream: the pool tree carries the
	// heavy pulls (distro install trees, install.wim) and a mid-transfer
	// drop on those restarts from byte zero unless Range is honored.
	serveRangeAware(c, f, st)
	return nil
}

// serveExternalKit streams the exported external PXE kit
// (MediaDir/netboot/external-tftp) as a tar archive: edge(原 edge) instances on
// the provisioning VLANs sync their TFTP root from here, so a kit refresh on
// the engine reaches every VLAN agent without a manual copy. Public like the
// other /netboot machine-face routes (the kit holds public NBP binaries and
// a trampoline whose engine URL is the deployment's own, docs/operations.md
// §pxe-external). 404 when the engine never exported a kit (PXEMode !=
// external and no directory on disk).
func (s *Server) serveExternalKit(c *gin.Context) {
	root := filepath.Join(s.MediaDir, "netboot", "external-tftp")
	st, err := os.Stat(root)
	if err != nil || !st.IsDir() {
		writeError(c, verrStatus(http.StatusNotFound, "RENDER_UNKNOWN_FILE",
			"no external kit exported — set MAMMOTH_PXE_MODE=external so the engine materializes MediaDir/netboot/external-tftp"))
		return
	}
	// The fingerprint (names+sizes+mtimes of the tree) doubles as the ETag:
	// content changes always move mtime, so a stat-based hash is a faithful
	// freshness signal without reading 8MB of bytes per probe.
	etag, ferr := kitFingerprint(root)
	if ferr != nil {
		etag = ""
	}
	if c.GetHeader("If-None-Match") != "" && c.GetHeader("If-None-Match") == etag {
		c.Status(http.StatusNotModified)
		return
	}
	c.Header("ETag", etag)
	obs.FromContext(c.Request.Context()).InfoContext(c.Request.Context(),
		"external kit served", "dir", root, "etag", etag)
	c.Header("Content-Type", "application/x-tar")
	c.Status(http.StatusOK)
	tw := tar.NewWriter(c.Writer)
	defer tw.Close()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		hdr, herr := tar.FileInfoHeader(info, "")
		if herr != nil {
			return herr
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, oerr := os.Open(path)
		if oerr != nil {
			return oerr
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		obs.FromContext(c.Request.Context()).ErrorContext(c.Request.Context(),
			"external kit tar failed mid-stream", "err", err.Error())
	}
}

// kitFingerprint hashes the kit tree's stat lines (relative name, size,
// mtime-nanos): a faithful freshness signal — any content change moves an
// mtime — without reading the tree's bytes. The walk is over a handful of
// NBP files, so this is microseconds.
func kitFingerprint(root string) (string, error) {
	h := sha256.New()
	werr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		fmt.Fprintf(h, "%s|%d|%d\n", rel, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if werr != nil {
		return "", werr
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ObservePXEClient records a PXE sighting reported by a provisioning-VLAN
// agent (edge): the agent sees every PXE DISCOVER on its L2, while the
// engine in external mode is blind to the wire — this endpoint is the
// external deployment's observation feed into pending_machines (mirrors the
// builtin mode's OnObserve hook). Public like the other /netboot
// machine-face routes.
func (s *Server) ObservePXEClient(c *gin.Context) {
	if s.Pending == nil {
		c.Status(http.StatusNoContent)
		return
	}
	var body struct {
		Mac, Firmware string
		Starved       bool
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	mac := netboot.NormalizeMAC(body.Mac)
	if mac == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "mac is not a hardware address"})
		return
	}
	if _, err := s.Pending.TouchByMAC(c.Request.Context(), mac, body.Firmware); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	payload := map[string]any{"firmware": body.Firmware, "source": "edge"}
	// Address-starvation marker (docs/09-roadmap.md「装机网段地址服务可
	// 观测」layer 2): the edge counted ≥N DISCOVERs from this MAC with no
	// addressing REQUEST — the provisioning segment's site DHCP is silent
	// for it. The event is the aggregation feed (submit-side preflight
	// reads the latest per-MAC marker through the events table).
	if body.Starved {
		payload["starved"] = true
	}
	s.Events.Append(c.Request.Context(), "pending", mac, "pending.sighted", payload)
	if body.Starved {
		obs.FromContext(c.Request.Context()).WarnContext(c.Request.Context(),
			"edge address-starvation sighting", "mac", mac,
			"cause", "repeated DISCOVER with no address REQUEST — the segment's site DHCP answered nothing")
	} else {
		obs.FromContext(c.Request.Context()).InfoContext(c.Request.Context(),
			"edge sighting recorded", "mac", mac, "firmware", body.Firmware)
	}
	c.Status(http.StatusNoContent)
}

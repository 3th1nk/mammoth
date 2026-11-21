package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/store"
)

// Image artifact library (docs/09-roadmap.md 下一阶段 6): registrations +
// the fetch worker's row states. Handlers are thin; the download lives in
// internal/images.

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// CreateImage registers an artifact and queues the fetch. The 202 matches
// the contract: fetching starts async (digest dedupe may complete it
// before the first poll even lands).
func (s *Server) CreateImage(ctx context.Context, request gen.CreateImageRequestObject) (gen.CreateImageResponseObject, error) {
	body := request.Body
	if body == nil {
		return nil, verr("SCHEMA_INVALID_IMAGE", "request body required")
	}
	if !strings.HasPrefix(body.SourceUrl, "http://") && !strings.HasPrefix(body.SourceUrl, "https://") {
		return nil, verr("SCHEMA_INVALID_IMAGE", "source_url must be http(s) — local files belong in spec.source, not the library")
	}
	if !sha256Hex.MatchString(body.Sha256) {
		return nil, verr("SCHEMA_INVALID_IMAGE", "sha256 must be a 64-char hex digest")
	}
	if s.Images == nil {
		return nil, verr("SCHEMA_INTERNAL", "image library not configured")
	}

	// Normalize the hand-filled identification at the door: the catalog's
	// distro column is what the spec fills verbatim, so a registration
	// saying "Kylin" must land as the canonical driver name (or at least
	// the lowercase family when no version can complete the join) — the
	// SetDetected backfill never overwrites a non-empty column.
	handDistro := derefOr(body.Distro, "")
	if s.Render != nil {
		handDistro = s.Render.CanonicalDistro(handDistro, derefOr(body.Version, ""))
	}
	im := &store.Image{
		ID:        store.NewID("img"),
		Name:      derefOr(body.Name, ""),
		SourceURL: body.SourceUrl,
		SHA256:    body.Sha256,
		Distro:    handDistro,
		Version:   derefOr(body.Version, ""),
		State:     "fetching",
	}
	if err := s.Images.Create(ctx, im); err != nil {
		return nil, err
	}
	// Re-read: the fetch sweep may already have completed a dedupe hit.
	if fresh, err := s.Images.Get(ctx, im.ID); err == nil {
		im = fresh
	}
	return gen.CreateImage202JSONResponse(imageOut(im)), nil
}

func (s *Server) ListImages(ctx context.Context, request gen.ListImagesRequestObject) (gen.ListImagesResponseObject, error) {
	rows, err := s.Images.List(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]gen.Image, 0, len(rows))
	for i := range rows {
		items = append(items, imageOut(&rows[i]))
	}
	return gen.ListImages200JSONResponse(gen.ImageList{Items: items}), nil
}

func (s *Server) GetImage(ctx context.Context, request gen.GetImageRequestObject) (gen.GetImageResponseObject, error) {
	im, err := s.Images.Get(ctx, string(request.Id))
	if err != nil {
		return nil, err
	}
	return gen.GetImage200JSONResponse(imageOut(im)), nil
}

// DeleteImage drops the registration and reclaims the cache file when no
// other registration shares the digest. Files for rows that were never
// ready simply don't exist.
func (s *Server) DeleteImage(ctx context.Context, request gen.DeleteImageRequestObject) (gen.DeleteImageResponseObject, error) {
	im, err := s.Images.Get(ctx, string(request.Id))
	if err != nil {
		return nil, err
	}
	if err := s.Images.Delete(ctx, im.ID); err != nil {
		return nil, err
	}
	if im.State == "ready" {
		if n, cerr := s.Images.CountBySHA(ctx, im.SHA256); cerr == nil && n == 0 {
			if p := im.FilePath; p != "" {
				// Best-effort, and only inside the cache root — the row's
				// path is engine-written, but belt and suspenders before rm.
				if s.ImagesDir == "" || filepath.Dir(p) == s.ImagesDir {
					_ = os.Remove(p)
				}
			}
		}
	}
	return gen.DeleteImage204Response{}, nil
}

func imageOut(im *store.Image) gen.Image {
	out := gen.Image{
		Id:        im.ID,
		Name:      &im.Name,
		SourceUrl: im.SourceURL,
		Sha256:    im.SHA256,
		Distro:    &im.Distro,
		Version:   &im.Version,
		State:     gen.ImageState(im.State),
		Error:     &im.Error,
		CreatedAt: im.CreatedAt,
		UpdatedAt: im.UpdatedAt,
	}
	if im.SizeBytes != 0 {
		out.SizeBytes = &im.SizeBytes
	}
	if im.Format != "" {
		format := gen.ImageFormat(im.Format)
		out.Format = &format
		out.RawSha256 = &im.RawSHA256
	}
	if im.RawSizeBytes != 0 {
		out.RawSizeBytes = &im.RawSizeBytes
	}
	return out
}

// specImageRef is the minimal slice of the install spec the resolver needs;
// everything else passes through untouched.
type specImageRef struct {
	Image struct {
		Id       string `json:"id"`
		Source   string `json:"source"`
		Checksum string `json:"checksum"`
		Distro   string `json:"distro"`
	} `json:"image"`
}

// resolveImageRefs rewrites spec.image.id into the concrete, checksum-gated
// cache source: source becomes the local file path, checksum the
// registration's digest (an explicit conflicting checksum is a rejection —
// the caller asked for bytes they claim differ from what they registered).
// Resolution snapshots into the job spec, so a later registration delete
// never affects submitted jobs. Specs without image.id pass through
// untouched.
func (s *Server) resolveImageRefs(ctx context.Context, specRaw json.RawMessage) (json.RawMessage, error) {
	var probe specImageRef
	if len(specRaw) == 0 || json.Unmarshal(specRaw, &probe) != nil {
		return specRaw, nil
	}
	if probe.Image.Id == "" {
		return specRaw, nil
	}
	if probe.Image.Source != "" {
		return nil, verr("SCHEMA_INVALID_SPEC", "image.id and image.source are mutually exclusive")
	}
	im, err := s.Images.Get(ctx, probe.Image.Id)
	if err != nil {
		return nil, verr("SCHEMA_INVALID_SPEC", "image.id %q not found in the library — register it first (POST /images)", probe.Image.Id)
	}
	switch im.State {
	case "ready":
	case "fetching":
		return nil, verr("SCHEMA_INVALID_SPEC", "image %q is still fetching — poll GET /images/%s until state=ready", im.ID, im.ID)
	default:
		return nil, verr("SCHEMA_INVALID_SPEC", "image %q failed to fetch: %s", im.ID, im.Error)
	}
	if probe.Image.Checksum != "" && probe.Image.Checksum != "sha256:"+im.SHA256 && probe.Image.Checksum != "sha256:"+im.RawSHA256 {
		return nil, verr("SCHEMA_INVALID_SPEC",
			"image %q: spec checksum %s conflicts with the registration's sha256:%s",
			im.ID, probe.Image.Checksum, im.SHA256)
	}

	var spec map[string]any
	if err := json.Unmarshal(specRaw, &spec); err != nil {
		return nil, verr("SCHEMA_INVALID_SPEC", "spec is not valid: %v", err)
	}
	img, _ := spec["image"].(map[string]any)
	if img == nil {
		return specRaw, nil
	}
	img["checksum"] = "sha256:" + im.SHA256
	delete(img, "id")
	// The dd pathway (docs/09「镜像直写通路」) streams from the MACHINE, not
	// the engine: source must be an http(s) URL the machine can reach — this
	// route. Every other consumer (installers, media assembly) reads the
	// engine-local cache path.
	if d, _ := img["distro"].(string); d == "dd" {
		base := strings.TrimSuffix(s.ExternalURL, "/")
		img["source"] = base + "/api/v1/images/" + probe.Image.Id + "/file"
		// The runtime verifies the UNCOMPRESSED stream (write-stream hash +
		// read-back span), so the resolution carries the uncompressed face
		// the fetch worker hashed out of the cache file — for raw files it
		// equals the registration's; legacy rows without detection fall
		// back to it (the only sound case there is an uncompressed image).
		rawSHA, rawSize := im.RawSHA256, im.RawSizeBytes
		if rawSHA == "" {
			rawSHA, rawSize = im.SHA256, im.SizeBytes
		}
		img["checksum"] = "sha256:" + rawSHA
		img["size_bytes"] = rawSize
		// Format is the runtime's decompressor choice: an explicit declaration
		// conflicting with the cache file's sniffed compression is a rejection
		// (the declared gunzip would corrupt the write stream); an omission
		// inherits the registration's detected format — facts beat defaults.
		if declared, _ := img["format"].(string); declared != "" && im.Format != "" && declared != im.Format {
			return nil, verr("SCHEMA_INVALID_SPEC",
				"image %q: spec format %s conflicts with the registered image's compression %s — the write stream is verified against the real bytes",
				im.ID, declared, im.Format)
		}
		if im.Format != "" {
			img["format"] = im.Format
		}
	} else {
		img["source"] = im.FilePath
	}
	out, merr := json.Marshal(spec)
	if merr != nil {
		return nil, verr("SCHEMA_INVALID_SPEC", "spec re-serialization failed: %v", merr)
	}
	return out, nil
}

// FetchImageFile serves the cached image file to the STREAMING MACHINE
// (docs/09「镜像直写通路」): the dd pathway's runtime pulls the whole-disk
// image over HTTP from this engine — the resolveImageRefs dd branch rewrites
// spec.image.source to this route's URL. Same trust model as the netboot
// file trees (the provisioning L2 is the boundary; no bearer token), same
// Range semantics (a multi-gigabyte image resuming after a dropped link
// continues at the last byte). state=ready is the gate: a fetching or
// failed row has no trustworthy bytes to serve.
func (s *Server) FetchImageFile(ctx context.Context, request gen.FetchImageFileRequestObject) (gen.FetchImageFileResponseObject, error) {
	im, err := s.Images.Get(ctx, request.Id)
	if err != nil {
		return nil, verrStatus(http.StatusNotFound, "SCHEMA_NOT_FOUND",
			"image %q not found in the library", request.Id)
	}
	if im.State != "ready" || im.FilePath == "" {
		return nil, verrStatus(http.StatusConflict, "IMAGE_NOT_READY",
			"image %q is not ready (state=%s) — poll GET /images/%s until state=ready",
			request.Id, im.State, request.Id)
	}
	f, err := os.Open(im.FilePath)
	if err != nil {
		return nil, verrStatus(http.StatusNotFound, "SCHEMA_NOT_FOUND",
			"cached file missing for image %q", request.Id)
	}
	// Range fetches answer straight on the gin writer and return a nil
	// response: the strict contract has no 206 shape (the same shape the
	// netboot file endpoint uses — see there for the full story).
	if gc := rangeFetch(ctx); gc != nil {
		st, serr := f.Stat()
		if serr != nil {
			f.Close()
			return nil, verrStatus(http.StatusInternalServerError, "IMAGE_STAT_FAILED", "stat failed: %s", serr.Error())
		}
		serveRangeAware(gc, f, st)
		f.Close()
		return nil, nil
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, verrStatus(http.StatusInternalServerError, "IMAGE_STAT_FAILED", "stat failed: %s", err.Error())
	}
	return gen.FetchImageFile200ApplicationoctetStreamResponse{
		Body:          f,
		ContentLength: st.Size(),
	}, nil
}

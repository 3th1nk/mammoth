package api

import (
	"context"
	"encoding/json"
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

	im := &store.Image{
		ID:        store.NewID("img"),
		Name:      derefOr(body.Name, ""),
		SourceURL: body.SourceUrl,
		SHA256:    body.Sha256,
		Distro:    derefOr(body.Distro, ""),
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
	if probe.Image.Checksum != "" && probe.Image.Checksum != "sha256:"+im.SHA256 {
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
	img["source"] = im.FilePath
	img["checksum"] = "sha256:" + im.SHA256
	delete(img, "id")
	out, merr := json.Marshal(spec)
	if merr != nil {
		return nil, verr("SCHEMA_INVALID_SPEC", "spec re-serialization failed: %v", merr)
	}
	return out, nil
}

package redfish

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakePatcher scripts the GET-ETag → PATCH rounds patchSystemBoot performs.
type fakePatcher struct {
	etagResponses  []string // ETag header per GET, in order
	patchStatuses  []int    // status per PATCH, in order
	seenIfMatch    []string // If-Match header of each PATCH actually sent
	getCall, patch int
}

func (f *fakePatcher) Get(url string) (*http.Response, error) {
	if f.getCall >= len(f.etagResponses) {
		etag := f.etagResponses[len(f.etagResponses)-1]
		f.getCall++
		return hdrResp(etag), nil
	}
	etag := f.etagResponses[f.getCall]
	f.getCall++
	return hdrResp(etag), nil
}

func hdrResp(etag string) *http.Response {
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	if etag != "" {
		resp.Header.Set("ETag", etag)
	}
	resp.Body = io.NopCloser(strings.NewReader(""))
	return resp
}

func (f *fakePatcher) PatchWithHeaders(url string, payload interface{}, headers map[string]string) (*http.Response, error) {
	f.seenIfMatch = append(f.seenIfMatch, headers["If-Match"])
	status := f.patchStatuses[min(f.patch, len(f.patchStatuses)-1)]
	f.patch++
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
}

// TestPatchSystemBoot412Retry pins the etag-race healing: a 412 (a KVM or
// console page advancing the iBMC resource version between GET and PATCH)
// triggers one refresh-and-retry with the NEW etag; a persisting storm
// falls through after the bounded retries.
func TestPatchSystemBoot412Retry(t *testing.T) {
	f := &fakePatcher{
		etagResponses: []string{`W/"v1"`, `W/"v2"`, `W/"v3"`},
		patchStatuses: []int{412, 412, 200},
	}
	resp, err := patchSystemBoot(f, "/redfish/v1/Systems/1", map[string]any{"BootSourceOverrideTarget": "Pxe"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("final status = %d, want 200", resp.StatusCode)
	}
	if f.patch != 3 {
		t.Fatalf("patches = %d, want 3 (two races then success)", f.patch)
	}
	// Each PATCH must carry the etag fetched immediately before it.
	want := []string{`W/"v1"`, `W/"v2"`, `W/"v3"`}
	for i, wm := range want {
		if f.seenIfMatch[i] != wm {
			t.Errorf("PATCH %d If-Match = %q, want %q", i+1, f.seenIfMatch[i], wm)
		}
	}
}

// TestPatchSystemBootClean pins the fast path: a 200 first PATCH sends
// exactly one PATCH with the one etag fetched.
func TestPatchSystemBootClean(t *testing.T) {
	f := &fakePatcher{
		etagResponses: []string{`W/"v1"`},
		patchStatuses: []int{200},
	}
	resp, err := patchSystemBoot(f, "/redfish/v1/Systems/1", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || f.patch != 1 {
		t.Fatalf("status = %d patches = %d; want 200/1", resp.StatusCode, f.patch)
	}
	if f.seenIfMatch[0] != `W/"v1"` {
		t.Errorf("If-Match = %q", f.seenIfMatch[0])
	}
}

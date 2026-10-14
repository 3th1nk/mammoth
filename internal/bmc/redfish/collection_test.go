package redfish

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/bmc"
)

func pageOf(ids ...string) string {
	var b strings.Builder
	b.WriteString(`{"Members":[`)
	for i, id := range ids {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"@odata.id":"` + id + `"}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

func TestFollowCollectionPages(t *testing.T) {
	// Vendors cap Members per page (75 is a common default) and continue
	// through Members@odata.nextLink — the walk must stitch pages in
	// collection order.
	tree := map[string]string{
		"/redfish/v1/Entries":         `{"Members":[{"@odata.id":"a"}],"Members@odata.nextLink":"/redfish/v1/Entries?$skip=1"}`,
		"/redfish/v1/Entries?$skip=1": `{"Members":[{"@odata.id":"b"},{"@odata.id":"c"}],"Members@odata.nextLink":"/redfish/v1/Entries?$skip=3"}`,
		"/redfish/v1/Entries?$skip=3": pageOf("d"),
		"/redfish/v1/empty":           `{"Members":[]}`,
		"/redfish/v1/count0":          `{"Members":[],"Members@odata.count":0}`,
	}
	get := func(url string) ([]byte, error) {
		if raw, ok := tree[url]; ok {
			return []byte(raw), nil
		}
		return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: "test", Detail: url}
	}

	got, err := followCollection(get, "test", "/redfish/v1/Entries")
	if err != nil {
		t.Fatalf("followCollection: %v", err)
	}
	want := []string{"a", "b", "c", "d"}
	if len(got) != len(want) {
		t.Fatalf("want %d members across 3 pages, got %d", len(want), len(got))
	}
	for i, w := range want {
		if got[i].ODataID != w {
			t.Fatalf("member %d: want %q, got %q", i, w, got[i].ODataID)
		}
	}

	// An empty first page is a valid collection, not an error.
	for _, url := range []string{"/redfish/v1/empty", "/redfish/v1/count0"} {
		if got, err = followCollection(get, "test", url); err != nil || len(got) != 0 {
			t.Fatalf("%s: want empty ok, got %v / %v", url, got, err)
		}
	}
}

func TestFollowCollectionFullURLNext(t *testing.T) {
	// Some firmwares publish the nextLink as a full URL; the HTTP layer
	// prepends the endpoint, so the chase must reduce it to path+query.
	tree := map[string]string{
		"/redfish/v1/Entries":         `{"Members":[{"@odata.id":"a"}],"Members@odata.nextLink":"https://bmc.example.com/redfish/v1/Entries?$skip=1"}`,
		"/redfish/v1/Entries?$skip=1": pageOf("b"),
	}
	get := func(url string) ([]byte, error) {
		if raw, ok := tree[url]; ok {
			return []byte(raw), nil
		}
		return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: "test", Detail: url}
	}
	got, err := followCollection(get, "test", "/redfish/v1/Entries")
	if err != nil {
		t.Fatalf("followCollection: %v", err)
	}
	if len(got) != 2 || got[1].ODataID != "b" {
		t.Fatalf("full-URL nextLink must be chased by path, got %+v", got)
	}
}

func TestFollowCollectionRelativeNextStops(t *testing.T) {
	// A bare-relative nextLink cannot be joined safely (no base to guess);
	// the chase stops with the pages it has.
	tree := map[string]string{
		"/redfish/v1/Entries": `{"Members":[{"@odata.id":"a"}],"Members@odata.nextLink":"Entries?$skip=1"}`,
	}
	get := func(url string) ([]byte, error) {
		if raw, ok := tree[url]; ok {
			return []byte(raw), nil
		}
		return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: "test", Detail: url}
	}
	got, err := followCollection(get, "test", "/redfish/v1/Entries")
	if err != nil {
		t.Fatalf("followCollection: %v", err)
	}
	if len(got) != 1 || got[0].ODataID != "a" {
		t.Fatalf("relative nextLink must stop the chase, got %+v", got)
	}
}

func TestFollowCollectionLoop(t *testing.T) {
	// A buggy firmware whose nextLink loops back must not wedge the caller.
	tree := map[string]string{
		"/redfish/v1/Entries":         `{"Members":[{"@odata.id":"a"}],"Members@odata.nextLink":"/redfish/v1/Entries?$skip=1"}`,
		"/redfish/v1/Entries?$skip=1": `{"Members":[{"@odata.id":"b"}],"Members@odata.nextLink":"/redfish/v1/Entries"}`,
	}
	get := func(url string) ([]byte, error) {
		if raw, ok := tree[url]; ok {
			return []byte(raw), nil
		}
		return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: "test", Detail: url}
	}
	got, err := followCollection(get, "test", "/redfish/v1/Entries")
	if err != nil {
		t.Fatalf("followCollection: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("loop must stop with gathered members, got %d", len(got))
	}
}

func TestFollowCollectionLaterPageFails(t *testing.T) {
	// A later page that dies degrades the read to the pages so far —
	// partial data beats no data.
	tree := map[string]string{
		"/redfish/v1/Entries":         `{"Members":[{"@odata.id":"a"}],"Members@odata.nextLink":"/redfish/v1/Entries?$skip=1"}`,
		"/redfish/v1/Entries?$skip=1": `{"Members":[{"@odata.id":"b"}],"Members@odata.nextLink":"/redfish/v1/Entries?$skip=2"}`,
	}
	get := func(url string) ([]byte, error) {
		if url == "/redfish/v1/Entries?$skip=2" {
			return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: "test", Detail: url}
		}
		if raw, ok := tree[url]; ok {
			return []byte(raw), nil
		}
		return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: "test", Detail: url}
	}
	got, err := followCollection(get, "test", "/redfish/v1/Entries")
	if err != nil {
		t.Fatalf("later-page failure must degrade, got %v", err)
	}
	if len(got) != 2 || got[1].ODataID != "b" {
		t.Fatalf("want pages 1-2 kept, got %+v", got)
	}
}

func TestFollowCollectionFirstPageFails(t *testing.T) {
	boom := &bmc.Error{Kind: bmc.KindUnreachable, Op: "test", Detail: "/redfish/v1/Entries"}
	get := func(string) ([]byte, error) { return nil, boom }
	if _, err := followCollection(get, "test", "/redfish/v1/Entries"); err != boom {
		t.Fatalf("first-page failure must surface the fetch error, got %v", err)
	}
}

func TestFollowCollectionFirstPageUndecodable(t *testing.T) {
	get := func(string) ([]byte, error) { return []byte(`{"Members":`), nil }
	_, err := followCollection(get, "health", "/redfish/v1/Entries")
	be, ok := err.(*bmc.Error)
	if !ok || be.Kind != bmc.KindProtocolError || be.Op != "health" {
		t.Fatalf("want protocol error under op health, got %v", err)
	}
}

func TestFollowCollectionLaterPageUndecodable(t *testing.T) {
	tree := map[string]string{
		"/redfish/v1/Entries":         `{"Members":[{"@odata.id":"a"}],"Members@odata.nextLink":"/redfish/v1/Entries?$skip=1"}`,
		"/redfish/v1/Entries?$skip=1": `{"Members":`,
	}
	get := func(url string) ([]byte, error) {
		if raw, ok := tree[url]; ok {
			return []byte(raw), nil
		}
		return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: "test", Detail: url}
	}
	got, err := followCollection(get, "test", "/redfish/v1/Entries")
	if err != nil || len(got) != 1 || got[0].ODataID != "a" {
		t.Fatalf("broken later page must keep page one, got %v / %+v", err, got)
	}
}

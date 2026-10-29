package redfish

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stmcginnis/gofish"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// ctxGet races the caller's deadline against the gofish client's GET — the
// regression it guards: gofish clients pin context.Background() (cached
// sessions), so without this race the registry deadline never aborts a long
// walk and callers (console SEL card) spin forever.

func ctxGetClient(t *testing.T, handler http.HandlerFunc, hits *atomic.Int64) *gofish.APIClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	// ConnectContext itself fetches the service root — that request (and any
	// count it adds) predates the ctxGet calls under test.
	c, err := gofish.ConnectContext(context.Background(), gofish.ClientConfig{
		Endpoint:   srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return c
}

func TestCtxGetDeadlineAbortsSlowGet(t *testing.T) {
	// Park only /slow until the test tears down (ConnectContext must still be
	// able to fetch the service root). The abandoned GET keeps its TCP
	// request alive server-side — ctxGet stops waiting, it does not cancel
	// the wire — so releasing the park at cleanup keeps the test instant.
	// Registered AFTER ctxGetClient: cleanup runs LIFO, so the park opens
	// before the server shut-down waits on the parked handler.
	park := make(chan struct{})
	c := ctxGetClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			select {
			case <-park:
			case <-r.Context().Done():
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}, nil)
	t.Cleanup(func() { close(park) })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := ctxGet(ctx, "sel", c)("/slow")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want deadline error, got nil")
	}
	var bmcErr *bmc.Error
	if !asBMCError(err, &bmcErr) || bmcErr.Kind != bmc.KindUnreachable {
		t.Fatalf("want BMC_UNREACHABLE, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ctx abort did not preempt the slow GET: %v", elapsed)
	}
}

func TestCtxGetPreCancelledSkipsRequest(t *testing.T) {
	var hits atomic.Int64
	c := ctxGetClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}, &hits)
	base := hits.Load()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := ctxGet(ctx, "sel", c)("/redfish/v1/"); err == nil {
		t.Fatal("want error on cancelled ctx, got nil")
	}
	if got := hits.Load(); got != base {
		t.Fatalf("cancelled ctx must not reach the wire: %d extra requests", got-base)
	}
}

func TestCtxGetPassesThrough(t *testing.T) {
	c := ctxGetClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}, nil)
	raw, err := ctxGet(context.Background(), "sel", c)("/redfish/v1/")
	if err != nil {
		t.Fatalf("want ok, got %v", err)
	}
	if string(raw) != `{"ok":true}` {
		t.Fatalf("want body passthrough, got %q", raw)
	}
}

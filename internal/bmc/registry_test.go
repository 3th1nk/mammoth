package bmc

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubDriver serves the Do-level tests; only Name and Probe are ever called
// (auto resolution probes, vendor metrics backfill).
type stubDriver struct {
	Driver // nil embed — unused methods must not be reached

	probeInFlight atomic.Int64
	probeMax      atomic.Int64
}

func (s *stubDriver) Name() Protocol { return ProtocolRedfish }

func (s *stubDriver) Probe(_ context.Context, _ string, _ Credentials) (BMCInfo, error) {
	cur := s.probeInFlight.Add(1)
	for {
		max := s.probeMax.Load()
		if cur <= max || s.probeMax.CompareAndSwap(max, cur) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	s.probeInFlight.Add(-1)
	return BMCInfo{Vendor: "stub"}, nil
}

// waitMapClean polls until every aborted acquire has handed its grant back
// and the map entry is gone (the return path is asynchronous by design).
func waitMapClean(t *testing.T, k *keyedMutex) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		k.mu.Lock()
		n := len(k.locks)
		k.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("keyed mutex still holds entries after all locks released")
}

func TestAcquireCtxPreCancelled(t *testing.T) {
	k := &keyedMutex{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := k.acquireCtx(ctx, "a"); err == nil {
		t.Fatal("want error on cancelled ctx, got nil")
	}
	waitMapClean(t, k)

	release, err := k.acquireCtx(context.Background(), "a")
	if err != nil {
		t.Fatalf("re-acquire after abort: %v", err)
	}
	release()
	waitMapClean(t, k)
}

func TestAcquireCtxAbandonWhileQueued(t *testing.T) {
	k := &keyedMutex{}
	holder, err := k.acquireCtx(context.Background(), "a")
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := k.acquireCtx(ctx, "a"); err == nil {
		t.Fatal("want abandon error while queued, got nil")
	}

	holder() // grant for the abandoned waiter may still land after this
	waitMapClean(t, k)

	release, err := k.acquireCtx(context.Background(), "a")
	if err != nil {
		t.Fatalf("re-acquire after abandon: %v", err)
	}
	release()
	waitMapClean(t, k)
}

// The regression the reorder guards: under the old order (budget minted
// before the lock wait) the queued caller's op would run on a deadline
// already eaten by the queue.
func TestDoBudgetStartsAfterLock(t *testing.T) {
	r := NewRegistry(nil, 2*time.Second)
	r.Register(&stubDriver{})

	entered := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = r.Do(context.Background(), "a", Credentials{}, ProtocolRedfish, "slow",
			func(_ context.Context, _ Driver) (any, error) {
				close(entered)
				<-release
				return nil, nil
			})
	}()
	<-entered

	// The queued Do below parks on the address lock; the holder is freed from
	// a timer goroutine — closing release from this thread would deadlock,
	// since this thread is the one parked in the queued Do.
	time.AfterFunc(300*time.Millisecond, func() { close(release) })

	var budget time.Duration
	_, err := r.Do(context.Background(), "a", Credentials{}, ProtocolRedfish, "queued",
		func(ctx context.Context, _ Driver) (any, error) {
			deadline, _ := ctx.Deadline()
			budget = time.Until(deadline)
			return nil, nil
		})
	if err != nil {
		t.Fatalf("queued Do: %v", err)
	}
	if budget < 1900*time.Millisecond {
		t.Fatalf("lock wait ate the op budget: %.0fms left of 2000ms", budget.Seconds()*1000)
	}
}

// auto resolution probes hit the BMC — under the old order they ran outside
// the per-address lock, so concurrent Do calls probed one controller in
// parallel, exactly what serialization exists to prevent.
func TestDoAutoProbesSerialize(t *testing.T) {
	r := NewRegistry(nil, 2*time.Second)
	stub := &stubDriver{}
	r.Register(stub)

	const n = 4
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Do(context.Background(), "a", Credentials{}, ProtocolAuto, "op",
				func(_ context.Context, _ Driver) (any, error) { return nil, nil }); err != nil {
				t.Errorf("Do: %v", err)
			}
		}()
	}
	wg.Wait()

	if max := stub.probeMax.Load(); max != 1 {
		t.Fatalf("auto probes ran concurrently: peak %d", max)
	}
}

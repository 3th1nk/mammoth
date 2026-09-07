package bmc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/3th1nk/mammoth/internal/obs"
)

// Registry resolves protocols to drivers and is the single call gateway:
// per-address serialization (a BMC is weak hardware — concurrent requests can
// wedge firmware), timeout isolation (one sick BMC must not hold up a runner),
// metrics and structured logging for every call (docs/07-bmc.md §5).
type Registry struct {
	mu      sync.RWMutex
	drivers map[Protocol]Driver
	vendors map[string]string // addr → vendor label cache for metrics

	locks *keyedMutex
	m     *obs.Metrics

	// DefaultTimeout caps every call without its own deadline.
	DefaultTimeout time.Duration
}

func NewRegistry(m *obs.Metrics, defaultTimeout time.Duration) *Registry {
	return &Registry{
		drivers:        map[Protocol]Driver{},
		locks:          &keyedMutex{},
		m:              m,
		DefaultTimeout: defaultTimeout,
	}
}

// Register installs a driver implementation.
func (r *Registry) Register(d Driver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drivers[d.Name()] = d
}

// Driver returns the registered implementation for a protocol.
func (r *Registry) Driver(p Protocol) (Driver, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.drivers[p]
	if !ok {
		return nil, &Error{Kind: KindUnsupported, Detail: fmt.Sprintf("no driver registered for protocol %q", p)}
	}
	return d, nil
}

// Resolve maps machine configuration to a concrete driver. `auto` probes
// Redfish first and falls back to IPMI — on most BMCs both protocols share
// address and credentials, so fallback costs no extra configuration
// (docs/07-bmc.md §2).
func (r *Registry) Resolve(ctx context.Context, addr string, cred Credentials, p Protocol) (Driver, error) {
	if p != ProtocolAuto {
		return r.Driver(p)
	}
	log := obs.FromContext(ctx)
	for _, candidate := range []Protocol{ProtocolRedfish, ProtocolIPMI} {
		d, err := r.Driver(candidate)
		if err != nil {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, r.DefaultTimeout)
		_, probeErr := r.call(probeCtx, d, addr, cred, "probe", func(ctx context.Context) (any, error) {
			return d.Probe(ctx, addr, cred)
		})
		cancel()
		if probeErr == nil {
			return d, nil
		}
		log.DebugContext(ctx, "protocol probe failed, trying next",
			obs.FieldBMCAddr, addr, "protocol", string(candidate), "err", probeErr.Error())
	}
	return nil, &Error{Kind: KindUnreachable, Op: "resolve", Detail: "no protocol answered (redfish, ipmi)"}
}

// Do runs fn against the driver selected for p, with auto-resolution,
// per-address serialization, a default timeout, and metrics/logging.
func (r *Registry) Do(ctx context.Context, addr string, cred Credentials, p Protocol, op string, fn func(ctx context.Context, d Driver) (any, error)) (any, error) {
	d, err := r.Resolve(ctx, addr, cred, p)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, r.DefaultTimeout)
	defer cancel()

	release := r.locks.acquire(addr)
	defer release()
	return r.call(callCtx, d, addr, cred, op, func(ctx context.Context) (any, error) {
		return fn(ctx, d)
	})
}

// call wraps one driver invocation with vendor-aware metrics and logging.
func (r *Registry) call(ctx context.Context, d Driver, addr string, cred Credentials, op string, fn func(ctx context.Context) (any, error)) (any, error) {
	ctx, span := obs.Tracer().Start(ctx, "bmc."+op)
	defer span.End()
	ctx = obs.With(ctx, obs.FieldBMCAddr, addr, obs.FieldOperation, op, "protocol", string(d.Name()))

	start := time.Now()
	res, err := fn(ctx)
	elapsed := time.Since(start)

	vendor := r.vendorFor(ctx, d, addr, cred)
	result := "ok"
	if err != nil {
		result = "error"
		var bmcErr *Error
		if asBMCError(err, &bmcErr) {
			if r.m != nil {
				r.m.BMCErrors.WithLabelValues(bmcErr.Code()).Inc()
			}
		}
	}
	if r.m != nil {
		r.m.BMCDuration.WithLabelValues(vendor, op, result).Observe(elapsed.Seconds())
	}

	log := obs.FromContext(ctx)
	if err != nil {
		log.ErrorContext(ctx, "bmc call failed",
			obs.FieldVendor, vendor, "err", err.Error(), "elapsed_ms", elapsed.Milliseconds())
	} else {
		log.DebugContext(ctx, "bmc call ok", obs.FieldVendor, vendor, "elapsed_ms", elapsed.Milliseconds())
	}
	return res, err
}

// vendorFor best-effort resolves a vendor label for metrics without paying a
// probe on every call: cached per address after the first successful probe.
func (r *Registry) vendorFor(ctx context.Context, d Driver, addr string, cred Credentials) string {
	r.mu.RLock()
	v, ok := r.vendors[addr]
	r.mu.RUnlock()
	if ok {
		return v
	}
	info, err := d.Probe(ctx, addr, cred)
	if err != nil || info.Vendor == "" {
		return "unknown"
	}
	r.mu.Lock()
	if r.vendors == nil {
		r.vendors = map[string]string{}
	}
	r.vendors[addr] = info.Vendor
	r.mu.Unlock()
	return info.Vendor
}

func asBMCError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

// keyedMutex serializes operations per key (per BMC address).
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*addressLock
}

type addressLock struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) acquire(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*addressLock{}
	}
	l, ok := k.locks[key]
	if !ok {
		l = &addressLock{}
		k.locks[key] = l
	}
	l.refs++
	k.mu.Unlock()

	l.mu.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Unlock()
			k.mu.Lock()
			l.refs--
			if l.refs == 0 {
				delete(k.locks, key)
			}
			k.mu.Unlock()
		})
	}
}

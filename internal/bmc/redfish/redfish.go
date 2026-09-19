// Package redfish implements bmc.Driver over the Redfish standard
// (github.com/stmcginnis/gofish). No vendor SDKs: standard HTTP + the
// published schema, OEM extensions enter later behind optional interfaces
// (docs/07-bmc.md §5).
package redfish

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/redfish"

	"github.com/3th1nk/mammoth/internal/bmc"
)

type Driver struct {
	// Insecure skips TLS verification (self-signed BMC certs are the norm).
	Insecure bool
	// Timeout bounds a single HTTP exchange against the BMC.
	Timeout time.Duration

	// Session cache: controllers rate-limit session creation (iBMC 6.41
	// answers 400 after a handful of rapid POSTs — a boot stage's mount /
	// eject / boot-set / power burst exhausts it and the stage dies with
	// BMC_AUTH_FAILED). Sessions are reused per address+user under a short
	// TTL; auth-kind operation failures invalidate the entry so the next
	// call reconnects. The Registry serializes per address, so a cached
	// client is never used concurrently.
	sessMu    sync.Mutex
	sessCache map[string]*sessEntry
}

// sessTTL bounds a cached client's reuse window (kept well under typical
// controller session idle timeouts).
const sessTTL = 5 * time.Minute

type sessEntry struct {
	client  *gofish.APIClient
	expires time.Time
}

func New(insecure bool, timeout time.Duration) *Driver {
	return &Driver{Insecure: insecure, Timeout: timeout, sessCache: map[string]*sessEntry{}}
}

// classify wraps bmc.Classify with session-cache invalidation: an auth-kind
// failure means the cached session died server-side — drop it so the next
// call reconnects instead of hammering a dead token.
func (d *Driver) classify(op string, err error) error {
	if e, ok := err.(*bmc.Error); ok && e.Kind == bmc.KindAuthFailed {
		// The cached session died server-side; drop everything —
		// reconnection is cheap relative to the failure.
		d.sessMu.Lock()
		d.sessCache = map[string]*sessEntry{}
		d.sessMu.Unlock()
	}
	return bmc.Classify(op, err)
}

func (d *Driver) Name() bmc.Protocol { return bmc.ProtocolRedfish }

// connect establishes an authenticated Redfish client. Session creation is
// vendor-aware: several BMCs extend the standard session payload with OEM
// fields (Huawei iBMC requires Oem.Huawei.Domain=LocaliBMC), so Mammoth owns
// the session handshake — vendor detection runs on the unauthenticated
// service root, the session POST carries the right shape, and gofish
// continues with the pre-authenticated token.
func (d *Driver) connect(ctx context.Context, addr string, cred bmc.Credentials) (*gofish.APIClient, error) {
	base := normalizeHost(addr)
	key := base + "|" + cred.Username

	d.sessMu.Lock()
	if e, ok := d.sessCache[key]; ok && time.Now().Before(e.expires) {
		cl := e.client
		d.sessMu.Unlock()
		return cl, nil
	}
	d.sessMu.Unlock()

	httpClient := makeHTTPClient(d.Timeout, d.Insecure)

	// The connection outlives the caller's context (cached clients are
	// reused across stages), so the handshake runs on a fresh one — gofish
	// binds the ConnectContext to every later request, and a caller-scoped
	// ctx would cancel all of them once its stage ends. Per-request
	// duration stays bounded by the HTTP client's Timeout.
	connCtx := context.Background()

	session, err := d.createSession(connCtx, httpClient, base, cred)
	if err != nil {
		return nil, err
	}

	cfg := gofish.ClientConfig{
		Endpoint:   base,
		Insecure:   d.Insecure,
		HTTPClient: httpClient,
	}
	if session.link != "" {
		cfg.Session = &gofish.Session{ID: session.link, Token: session.token}
	} else {
		// No SessionService (rare): basic auth remains the only door.
		cfg.Username = cred.Username
		cfg.Password = cred.Password
		cfg.BasicAuth = true
	}
	client, err := gofish.ConnectContext(connCtx, cfg)
	if err != nil {
		return nil, bmc.Classify("connect", err)
	}
	d.sessMu.Lock()
	d.sessCache[key] = &sessEntry{client: client, expires: time.Now().Add(sessTTL)}
	d.sessMu.Unlock()
	return client, nil
}

// redfishSession is one established controller session.
type redfishSession struct {
	token string
	link  string
}

// createSession performs the vendor-aware session handshake. The service
// root is read unauthenticated to identify the vendor via Oem; non-Huawei
// BMCs take the standard payload. 404/405 on the session endpoint signals
// "no session service" (empty link → basic auth fallback).
func (d *Driver) createSession(ctx context.Context, httpClient *http.Client, base string, cred bmc.Credentials) (*redfishSession, error) {
	const op = "session"
	rootReq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/redfish/v1/", nil)
	if err != nil {
		return nil, d.classify(op, err)
	}
	rootResp, err := httpClient.Do(rootReq)
	if err != nil {
		return nil, d.classify(op, err)
	}
	defer rootResp.Body.Close()
	rootRaw, _ := io.ReadAll(rootResp.Body)

	payload := map[string]any{"UserName": cred.Username, "Password": cred.Password}
	var root struct {
		Oem struct {
			Huawei json.RawMessage `json:"Huawei"`
		} `json:"Oem"`
	}
	if json.Unmarshal(rootRaw, &root) == nil && len(root.Oem.Huawei) > 0 {
		payload["Oem"] = map[string]any{
			"Huawei": map[string]any{"Domain": "LocaliBMC"},
		}
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/redfish/v1/SessionService/Sessions", bytes.NewReader(body))
	if err != nil {
		return nil, d.classify(op, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, d.classify(op, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		token := resp.Header.Get("X-Auth-Token")
		link := resp.Header.Get("Location")
		if token == "" || link == "" {
			return nil, &bmc.Error{Kind: bmc.KindProtocolError, Op: op,
				Detail: "session created without token/location headers"}
		}
		return &redfishSession{token: token, link: link}, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return &redfishSession{}, nil // no session service → caller uses basic auth
	case resp.StatusCode >= 500:
		raw, _ := io.ReadAll(resp.Body)
		return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: op,
			Detail: fmt.Sprintf("session creation: %s %s", resp.Status, bmc.FirstLine(string(raw)))}
	default:
		raw, _ := io.ReadAll(resp.Body)
		return nil, &bmc.Error{Kind: bmc.KindAuthFailed, Op: op,
			Detail: fmt.Sprintf("session creation: %s %s", resp.Status, bmc.FirstLine(string(raw)))}
	}
}

func (d *Driver) Probe(ctx context.Context, addr string, cred bmc.Credentials) (bmc.BMCInfo, error) {
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return bmc.BMCInfo{}, err
	}

	info := bmc.BMCInfo{Protocol: bmc.ProtocolRedfish, PowerState: bmc.PowerStateUnknown}

	if systems, err := c.Service.Systems(); err == nil && len(systems) > 0 {
		sys := systems[0]
		info.Vendor = strings.ToLower(sys.Manufacturer)
		info.Model = sys.Model
		info.SerialNumber = sys.SerialNumber
		switch redfish.PowerState(sys.PowerState) {
		case redfish.OnPowerState:
			info.PowerState = bmc.PowerStateOn
		case redfish.OffPowerState:
			info.PowerState = bmc.PowerStateOff
		}
	}
	if mgrs, err := c.Service.Managers(); err == nil && len(mgrs) > 0 {
		info.FirmwareVersion = mgrs[0].FirmwareVersion
	}
	if info.Vendor == "" && info.Model == "" && info.FirmwareVersion == "" {
		return info, &bmc.Error{Kind: bmc.KindProtocolError, Op: "probe",
			Detail: "service root answered but carries no system/manager data"}
	}
	return info, nil
}

func (d *Driver) PowerState(ctx context.Context, addr string, cred bmc.Credentials) (bmc.PowerState, error) {
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return bmc.PowerStateUnknown, err
	}

	systems, err := c.Service.Systems()
	if err != nil {
		return bmc.PowerStateUnknown, bmc.Classify("power_state", err)
	}
	if len(systems) == 0 {
		return bmc.PowerStateUnknown, &bmc.Error{Kind: bmc.KindProtocolError, Op: "power_state", Detail: "no computer system resource"}
	}
	switch redfish.PowerState(systems[0].PowerState) {
	case redfish.OnPowerState:
		return bmc.PowerStateOn, nil
	case redfish.OffPowerState:
		return bmc.PowerStateOff, nil
	default:
		return bmc.PowerStateUnknown, nil
	}
}

func (d *Driver) SetPower(ctx context.Context, addr string, cred bmc.Credentials, action bmc.PowerAction) error {
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return err
	}

	resetType, ok := resetTypeFor(action)
	if !ok {
		return &bmc.Error{Kind: bmc.KindUnsupported, Op: "set_power",
			Detail: fmt.Sprintf("action %q has no Redfish ResetType mapping", action)}
	}
	systems, err := c.Service.Systems()
	if err != nil {
		return bmc.Classify("set_power", err)
	}
	if len(systems) == 0 {
		return &bmc.Error{Kind: bmc.KindProtocolError, Op: "set_power", Detail: "no computer system resource"}
	}
	// Pre-read the Reset action's allowed values (sushy does the same):
	// restrictive firmwares accept only a subset of the standard ResetTypes
	// (Huawei iBMC 6.41: PowerCycle/GracefulRestart rejected in favor of
	// ForceRestart, ForceOn in favor of On — docs/compat/huawei.md §8).
	// Sending only values the firmware claims to accept avoids a failed
	// round-trip; the runtime fallback below stays as the belt to the
	// announcement's braces.
	if allowed := resetAllowedValues(c, systems[0]); len(allowed) > 0 {
		if !allowed[string(resetType)] {
			if alt, ok := resetTypeFallback(resetType); ok && allowed[string(alt)] {
				resetType = alt
			}
		}
	}
	if err := systems[0].Reset(resetType); err != nil {
		// Retry once with the alternate mapping when the firmware rejects
		// the sent ResetType despite (or without) its announcement.
		if alt, ok := resetTypeFallback(resetType); ok && isResetTypeFormatError(err) {
			if serr := systems[0].Reset(alt); serr == nil {
				return nil
			}
		}
		return bmc.Classify("set_power", err)
	}
	return nil
}

// resetAllowedValues reads the Reset action's
// ResetType@Redfish.AllowableValues announcement; empty means the firmware
// publishes nothing useful and any standard value is worth a try.
func resetAllowedValues(c *gofish.APIClient, sys *redfish.ComputerSystem) map[string]bool {
	resp, err := c.Get(sys.ODataID)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}
	var doc struct {
		Actions struct {
			Reset struct {
				Allowed []string `json:"ResetType@Redfish.AllowableValues"`
			} `json:"#ComputerSystem.Reset"`
		} `json:"Actions"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	if len(doc.Actions.Reset.Allowed) == 0 {
		return nil
	}
	out := make(map[string]bool, len(doc.Actions.Reset.Allowed))
	for _, v := range doc.Actions.Reset.Allowed {
		out[v] = true
	}
	return out
}

// resetTypeFallback maps ResetTypes onto the values restrictive firmwares
// accept for the same operation.
func resetTypeFallback(rt redfish.ResetType) (redfish.ResetType, bool) {
	switch rt {
	case redfish.PowerCycleResetType, redfish.GracefulRestartResetType:
		return redfish.ForceRestartResetType, true
	case redfish.ForceOnResetType:
		return redfish.OnResetType, true
	}
	return "", false
}

// isResetTypeFormatError reports whether err is the Redfish
// ActionParameterValueFormatError rejection of the sent ResetType.
func isResetTypeFormatError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "ActionParameterValueFormatError")
}

// resetTypeFor maps unified power actions onto standard Redfish ResetType.
func resetTypeFor(a bmc.PowerAction) (redfish.ResetType, bool) {
	switch a {
	case bmc.PowerOn:
		return redfish.ForceOnResetType, true
	case bmc.PowerOff:
		return redfish.ForceOffResetType, true
	case bmc.SoftOff:
		return redfish.GracefulShutdownResetType, true
	case bmc.HardReboot:
		return redfish.ForceRestartResetType, true
	case bmc.SoftReboot:
		return redfish.GracefulRestartResetType, true
	case bmc.Cycle:
		return redfish.PowerCycleResetType, true
	default:
		return "", false
	}
}

func (d *Driver) SetBootDevice(ctx context.Context, addr string, cred bmc.Credentials, dev bmc.BootDevice, once bool) error {
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return err
	}

	target, ok := bootTargetFor(dev)
	if !ok {
		return &bmc.Error{Kind: bmc.KindUnsupported, Op: "set_boot_device",
			Detail: fmt.Sprintf("device %q has no Redfish mapping", dev)}
	}
	systems, err := c.Service.Systems()
	if err != nil {
		return bmc.Classify("set_boot_device", err)
	}
	if len(systems) == 0 {
		return &bmc.Error{Kind: bmc.KindProtocolError, Op: "set_boot_device", Detail: "no computer system resource"}
	}
	sys := systems[0]
	// PATCH the Boot object directly instead of gofish's SetBoot: some
	// firmware (Huawei iBMC 6.41 observed) requires If-Match on the main
	// resource and has no Settings object — gofish falls back to that
	// missing object with an error message that buries the real 412.
	// Carrying the resource's own ETag when published is the sushy-proven
	// form; absent an ETag the plain PATCH stays the first attempt.
	boot := map[string]any{
		"BootSourceOverrideTarget":  target,
		"BootSourceOverrideEnabled": enabledFor(once),
	}
	if sys.Boot.BootSourceOverrideMode != "" {
		boot["BootSourceOverrideMode"] = sys.Boot.BootSourceOverrideMode
	}
	resp, err := patchSystemBoot(c, sys.ODataID, boot)
	if err != nil {
		return bmc.Classify("set_boot_device", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return &bmc.Error{Kind: bmc.KindProtocolError, Op: "set_boot_device",
			Detail: fmt.Sprintf("boot override PATCH: %s %s", resp.Status, bmc.FirstLine(string(raw)))}
	}
	return nil
}

func enabledFor(once bool) string {
	if once {
		return string(redfish.OnceBootSourceOverrideEnabled)
	}
	return string(redfish.ContinuousBootSourceOverrideEnabled)
}

// patchSystemBoot PATCHes the Boot object, carrying the resource's ETag as
// If-Match when the firmware publishes one (409 PreconditionFailed otherwise).
func patchSystemBoot(c *gofish.APIClient, systemURL string, boot map[string]any) (*http.Response, error) {
	headers := map[string]string{}
	if resp, err := c.Get(systemURL); err == nil {
		// Pass the published ETag through verbatim (including any weak
		// `W/` prefix — iBMC 6.41 both publishes and requires exactly this
		// form; verified by a live 200 on the value its GET returned).
		etag := resp.Header.Get("ETag")
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if etag != "" {
			headers["If-Match"] = etag
		}
	}
	payload := map[string]any{"Boot": boot}
	return c.PatchWithHeaders(systemURL, payload, headers)
}

func bootTargetFor(d bmc.BootDevice) (redfish.BootSourceOverrideTarget, bool) {
	switch d {
	case bmc.BootPXE:
		return redfish.PxeBootSourceOverrideTarget, true
	case bmc.BootDisk:
		return redfish.HddBootSourceOverrideTarget, true
	case bmc.BootCDROM:
		return redfish.CdBootSourceOverrideTarget, true
	case bmc.BootBIOS:
		return redfish.BiosSetupBootSourceOverrideTarget, true
	default:
		return "", false
	}
}

func (d *Driver) MountMedia(ctx context.Context, addr string, cred bmc.Credentials, img bmc.MediaImage) error {
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return err
	}

	vms, verr := managersVirtualMedia(c)
	if verr != nil {
		return verr
	}
	if len(vms) == 0 {
		return &bmc.Error{Kind: bmc.KindUnsupported, Op: "mount_media", Detail: "no virtual media resource"}
	}
	// Slot fallback (the pattern Ironic's redfish boot interface is built
	// on): slots vary wildly — Cisco ships KVM-only and internal-use slots
	// that reject inserts, OpenBMC slots may lack the action entirely — so
	// a rejected slot falls through to the next candidate instead of
	// failing the operation.
	for _, vm := range orderMediaSlots(vms) {
		if !vm.SupportsMediaInsert {
			continue
		}
		if err := vm.InsertMedia(img.URL, true, true); err != nil {
			continue // next candidate slot
		}
		return nil
	}
	// No action worked — or none is advertised. Several BMCs accept a
	// plain PATCH of the resource instead (Image + Inserted), which is the
	// standards-track insert form for them.
	for _, vm := range orderMediaSlots(vms) {
		vm.Image = img.URL
		vm.Inserted = true
		vm.WriteProtected = true
		if err := vm.Update(); err == nil {
			return nil
		}
	}
	// Vendor OEM path last (Huawei iBMC observed: VmmControl only, with
	// task polling until the mount settles).
	return d.vmmControl(ctx, c, addr, img.URL, "Connect")
}

// orderMediaSlots prefers CD slots, then DVD (some firmware — Cisco UCS —
// ships only a DVD slot), everything else last.
func orderMediaSlots(vms []*redfish.VirtualMedia) []*redfish.VirtualMedia {
	rank := func(vm *redfish.VirtualMedia) int {
		for _, t := range vm.MediaTypes {
			switch t {
			case "CD":
				return 0
			case "DVD":
				return 1
			}
		}
		return 2
	}
	ordered := append([]*redfish.VirtualMedia(nil), vms...)
	sort.SliceStable(ordered, func(i, j int) bool { return rank(ordered[i]) < rank(ordered[j]) })
	return ordered
}

func (d *Driver) EjectMedia(ctx context.Context, addr string, cred bmc.Credentials, img bmc.MediaImage) error {
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return err
	}

	vms, verr := managersVirtualMedia(c)
	if verr != nil || len(vms) == 0 {
		return &bmc.Error{Kind: bmc.KindUnsupported, Op: "eject_media", Detail: "no virtual media resource"}
	}
	advertised := false
	for _, vm := range orderMediaSlots(vms) {
		if !vm.SupportsMediaEject {
			continue
		}
		advertised = true
		if !vm.Inserted {
			continue
		}
		// Slot-matching: eject only the slot carrying the target image.
		if img.URL != "" && vm.Image != "" && vm.Image != img.URL {
			continue
		}
		if err := vm.EjectMedia(); err != nil {
			// A just-issued eject can still be in flight on some firmware
			// (Dell: the next insert would 500 until it settles) — retry
			// once before falling through.
			time.Sleep(3 * time.Second)
			if err2 := vm.EjectMedia(); err2 != nil {
				return bmc.Classify("eject_media", err2)
			}
			return nil
		}
		return nil
	}
	// PATCH form for action-less slots.
	for _, vm := range orderMediaSlots(vms) {
		if !vm.Inserted {
			continue
		}
		vm.Inserted = false
		vm.Image = ""
		if err := vm.Update(); err == nil {
			return nil
		}
	}
	if !advertised {
		// Huawei-style: no eject action advertised → OEM VmmControl Disconnect.
		return d.vmmControl(ctx, c, addr, img.URL, "Disconnect")
	}
	return nil
}

func managersVirtualMedia(c *gofish.APIClient) ([]*redfish.VirtualMedia, error) {
	managers, err := c.Service.Managers()
	if err != nil {
		return nil, bmc.Classify("virtual_media", err)
	}
	var all []*redfish.VirtualMedia
	for _, m := range managers {
		vms, err := m.VirtualMedia()
		if err != nil {
			// surface the walk error — an empty collection is
			// indistinguishable from a dead session otherwise
			return nil, bmc.Classify("virtual_media", err)
		}
		all = append(all, vms...)
	}
	return all, nil
}

// ConsoleURL is OEM territory: no standard Redfish KVM resource exists. Known
// OEM mappings enter behind an OEMExtensions interface; until then report
// unsupported so callers degrade gracefully (docs/07-bmc.md §5).
func (d *Driver) ConsoleURL(_ context.Context, _ string, _ bmc.Credentials) (string, error) {
	return "", &bmc.Error{Kind: bmc.KindUnsupported, Op: "console_url",
		Detail: "no OEM KVM mapping for this vendor yet"}
}

// normalizeHost accepts bare hosts, host:port and URL forms.
func normalizeHost(addr string) string {
	if strings.Contains(addr, "://") {
		return strings.TrimSuffix(addr, "/")
	}
	return "https://" + addr
}

// makeHTTPClient builds the transport used for both the session handshake
// and gofish resource walks. InsecureSkipVerify is opt-in per deployment —
// BMC self-signed certificates are the norm in the field
// (MAMMOTH_BMC_TLS_INSECURE).
func makeHTTPClient(timeout time.Duration, insecure bool) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	transport := &http.Transport{}
	if insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 — deployment opt-in
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

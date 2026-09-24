package redfish

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// Health samples the controller's live health snapshot (docs/07-bmc.md
// §6.3): chassis power state + every sensor the Chassis collection reports
// (fans, temperatures, power supplies, voltages), each with the Redfish
// health descriptor normalized to ok|warning|critical|unknown.
func (d *Driver) Health(ctx context.Context, addr string, cred bmc.Credentials) (bmc.HealthView, error) {
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return bmc.HealthView{}, err
	}

	get := func(url string) ([]byte, error) {
		resp, err := c.Get(url)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: "health",
				Detail: url + ": status " + resp.Status}
		}
		return raw, nil
	}
	return walkHealth(get)
}

// walkHealth follows Service root → Chassis collection → each chassis
// (Thermal + Power resources) with the fetcher injected (unit-testable
// against canned JSON; the live driver passes the gofish client's GET).
// The lenient-processor discipline applies at every hop: a chassis without
// Thermal/Power links, or a broken sensor entry, degrades the sample —
// never fails it.
func walkHealth(get func(string) ([]byte, error)) (bmc.HealthView, error) {
	rootRaw, err := get("/redfish/v1/")
	if err != nil {
		return bmc.HealthView{}, err
	}
	var root struct {
		Chassis *odataLink `json:"Chassis"`
	}
	if json.Unmarshal(rootRaw, &root) != nil || root.Chassis == nil || root.Chassis.ODataID == "" {
		return bmc.HealthView{}, &bmc.Error{Kind: bmc.KindUnsupported, Op: "health",
			Detail: "service root carries no Chassis collection"}
	}
	collRaw, err := get(root.Chassis.ODataID)
	if err != nil {
		return bmc.HealthView{}, err
	}
	var coll struct {
		Members []odataLink `json:"Members"`
	}
	if json.Unmarshal(collRaw, &coll) != nil {
		return bmc.HealthView{}, &bmc.Error{Kind: bmc.KindProtocolError, Op: "health",
			Detail: "Chassis collection undecodable"}
	}

	view := bmc.HealthView{Health: bmc.SensorOK, Sensors: []bmc.SensorReading{}}
	seen := false // any health verdict at all — without one, overall is unknown
	worst := func(s bmc.SensorState) {
		seen = seen || s != bmc.SensorUnknown
		switch s {
		case bmc.SensorCritical:
			view.Health = bmc.SensorCritical
		case bmc.SensorWarning:
			if view.Health != bmc.SensorCritical {
				view.Health = bmc.SensorWarning
			}
		}
	}

	for _, m := range coll.Members {
		if m.ODataID == "" {
			continue
		}
		raw, err := get(m.ODataID)
		if err != nil {
			continue // one unreachable chassis must not fail the sample
		}
		// Chassis frame: only the consumed fields — vendors violate the
		// spec elsewhere and that must not matter here.
		var chassis struct {
			PowerState string `json:"PowerState"`
			Status     struct {
				Health string `json:"Health"`
			} `json:"Status"`
			Thermal *odataLink `json:"Thermal"`
			Power   *odataLink `json:"Power"`
		}
		if json.Unmarshal(raw, &chassis) != nil {
			continue
		}
		switch strings.ToLower(chassis.PowerState) {
		case "on":
			view.PowerState = bmc.PowerStateOn
		case "off":
			view.PowerState = bmc.PowerStateOff
		}
		worst(normalizeHealth(chassis.Status.Health))

		if chassis.Thermal != nil && chassis.Thermal.ODataID != "" {
			if raw, err := get(chassis.Thermal.ODataID); err == nil {
				var th struct {
					Fans []struct {
						Name         string  `json:"Name"`
						Reading      float64 `json:"Reading"`
						ReadingUnits string  `json:"ReadingUnits"`
						Status       struct {
							Health string `json:"Health"`
						} `json:"Status"`
					} `json:"Fans"`
					Temperatures []struct {
						Name           string  `json:"Name"`
						ReadingCelsius float64 `json:"ReadingCelsius"`
						Status         struct {
							Health string `json:"Health"`
						} `json:"Status"`
					} `json:"Temperatures"`
				}
				if json.Unmarshal(raw, &th) == nil {
					for _, f := range th.Fans {
						view.Sensors = append(view.Sensors, bmc.SensorReading{
							Name:    f.Name,
							Reading: f.Reading,
							Unit:    unitOr(f.ReadingUnits, "RPM"),
							State:   normalizeHealth(f.Status.Health),
						})
						worst(normalizeHealth(f.Status.Health))
					}
					for _, t := range th.Temperatures {
						view.Sensors = append(view.Sensors, bmc.SensorReading{
							Name:    t.Name,
							Reading: t.ReadingCelsius,
							Unit:    "Celsius",
							State:   normalizeHealth(t.Status.Health),
						})
						worst(normalizeHealth(t.Status.Health))
					}
				}
			}
		}

		if chassis.Power != nil && chassis.Power.ODataID != "" {
			if raw, err := get(chassis.Power.ODataID); err == nil {
				var pw struct {
					PowerSupplies []struct {
						Name                 string  `json:"Name"`
						LastPowerOutputWatts float64 `json:"LastPowerOutputWatts"`
						Status               struct {
							Health string `json:"Health"`
						} `json:"Status"`
					} `json:"PowerSupplies"`
					Voltages []struct {
						Name         string  `json:"Name"`
						ReadingVolts float64 `json:"ReadingVolts"`
						Status       struct {
							Health string `json:"Health"`
						} `json:"Status"`
					} `json:"Voltages"`
				}
				if json.Unmarshal(raw, &pw) == nil {
					for _, p := range pw.PowerSupplies {
						view.Sensors = append(view.Sensors, bmc.SensorReading{
							Name:    p.Name,
							Reading: p.LastPowerOutputWatts,
							Unit:    "Watts",
							State:   normalizeHealth(p.Status.Health),
						})
						worst(normalizeHealth(p.Status.Health))
					}
					for _, v := range pw.Voltages {
						view.Sensors = append(view.Sensors, bmc.SensorReading{
							Name:    v.Name,
							Reading: v.ReadingVolts,
							Unit:    "Volts",
							State:   normalizeHealth(v.Status.Health),
						})
						worst(normalizeHealth(v.Status.Health))
					}
				}
			}
		}
	}
	if !seen {
		view.Health = bmc.SensorUnknown
	}
	return view, nil
}

// normalizeHealth maps a Redfish Status.Health value onto SensorState.
// Anything unparseable — empty, "Absent", vendor junk — is unknown: an
// unreadable descriptor never invents a warning.
func normalizeHealth(s string) bmc.SensorState {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ok":
		return bmc.SensorOK
	case "warning":
		return bmc.SensorWarning
	case "critical":
		return bmc.SensorCritical
	default:
		return bmc.SensorUnknown
	}
}

// unitOr fills the fan unit when the vendor omits ReadingUnits.
func unitOr(unit, fallback string) string {
	if strings.TrimSpace(unit) == "" {
		return fallback
	}
	return unit
}

// SystemEventLog retrieves the controller's system event log (docs
// 07-bmc.md §6.3): every manager's log services concatenated, newest
// first, capped at bmc.SELMaxEntries.
func (d *Driver) SystemEventLog(ctx context.Context, addr string, cred bmc.Credentials) ([]bmc.SELEntry, error) {
	c, err := d.connect(ctx, addr, cred)
	if err != nil {
		return nil, err
	}

	get := func(url string) ([]byte, error) {
		resp, err := c.Get(url)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: "sel",
				Detail: url + ": status " + resp.Status}
		}
		return raw, nil
	}
	return walkSEL(get)
}

// walkSEL follows Service root → Managers → each manager's LogServices →
// Entries collections with the fetcher injected. Log collections are
// append-ordered oldest→newest (DSP0268 LogService semantics; ring buffers
// wrap but keep the order), so the cap takes the collection tail before
// fetching entry bodies — one GET for the links, bmc.SELMaxEntries GETs at
// most for content. A log service without Entries, or a broken entry,
// degrades the read — never fails it.
func walkSEL(get func(string) ([]byte, error)) ([]bmc.SELEntry, error) {
	rootRaw, err := get("/redfish/v1/")
	if err != nil {
		return nil, err
	}
	var root struct {
		Managers *odataLink `json:"Managers"`
	}
	if json.Unmarshal(rootRaw, &root) != nil || root.Managers == nil || root.Managers.ODataID == "" {
		return nil, &bmc.Error{Kind: bmc.KindUnsupported, Op: "sel",
			Detail: "service root carries no Managers collection"}
	}
	mgrCollRaw, err := get(root.Managers.ODataID)
	if err != nil {
		return nil, err
	}
	var mgrColl struct {
		Members []odataLink `json:"Members"`
	}
	if json.Unmarshal(mgrCollRaw, &mgrColl) != nil {
		return nil, &bmc.Error{Kind: bmc.KindProtocolError, Op: "sel",
			Detail: "Managers collection undecodable"}
	}

	var entries []bmc.SELEntry
	for _, mgr := range mgrColl.Members {
		if mgr.ODataID == "" {
			continue
		}
		mgrRaw, err := get(mgr.ODataID)
		if err != nil {
			continue
		}
		var m struct {
			LogServices *odataLink `json:"LogServices"`
		}
		if json.Unmarshal(mgrRaw, &m) != nil || m.LogServices == nil || m.LogServices.ODataID == "" {
			continue
		}
		lsCollRaw, err := get(m.LogServices.ODataID)
		if err != nil {
			continue
		}
		var lsColl struct {
			Members []odataLink `json:"Members"`
		}
		if json.Unmarshal(lsCollRaw, &lsColl) != nil {
			continue
		}
		for _, ls := range lsColl.Members {
			if ls.ODataID == "" {
				continue
			}
			entries = append(entries, walkSELService(get, ls.ODataID)...)
		}
	}

	// Newest first; ties break by id so the order is stable across reads.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Timestamp != entries[j].Timestamp {
			return entries[i].Timestamp > entries[j].Timestamp
		}
		return entries[i].ID > entries[j].ID
	})
	if len(entries) > bmc.SELMaxEntries {
		entries = entries[:bmc.SELMaxEntries]
	}
	return entries, nil
}

// walkSELService reads one log service's entries collection.
func walkSELService(get func(string) ([]byte, error), serviceURL string) []bmc.SELEntry {
	svcRaw, err := get(serviceURL)
	if err != nil {
		return nil
	}
	var svc struct {
		Entries *odataLink `json:"Entries"`
	}
	if json.Unmarshal(svcRaw, &svc) != nil || svc.Entries == nil || svc.Entries.ODataID == "" {
		return nil
	}
	collRaw, err := get(svc.Entries.ODataID)
	if err != nil {
		return nil
	}
	var coll struct {
		Members []odataLink `json:"Members"`
	}
	if json.Unmarshal(collRaw, &coll) != nil {
		return nil
	}

	// Collection-order tail: the newest records live at the end.
	links := coll.Members
	if len(links) > bmc.SELMaxEntries {
		links = links[len(links)-bmc.SELMaxEntries:]
	}

	out := make([]bmc.SELEntry, 0, len(links))
	for _, link := range links {
		if link.ODataID == "" {
			continue
		}
		raw, err := get(link.ODataID)
		if err != nil {
			continue // presence without detail beats failing the log
		}
		var item struct {
			ID       string `json:"Id"`
			Created  string `json:"Created"`
			Severity string `json:"Severity"`
			Message  string `json:"Message"`
		}
		if json.Unmarshal(raw, &item) != nil || item.ID == "" {
			continue
		}
		out = append(out, bmc.SELEntry{
			ID:        item.ID,
			Timestamp: item.Created,
			Severity:  normalizeSeverity(item.Severity),
			Message:   strings.TrimSpace(item.Message),
		})
	}
	return out
}

// normalizeSeverity maps a Redfish LogEntry.Severity onto the contract's
// vocabulary. Empty and vendor-specific values ("Debug", OEM strings) are
// unknown rather than silently regraded as OK.
func normalizeSeverity(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ok":
		return "ok"
	case "warning":
		return "warning"
	case "critical":
		return "critical"
	default:
		return "unknown"
	}
}

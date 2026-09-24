package redfish

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/bmc"
)

// walkHealth follows Service root → Chassis collection → each chassis's
// Thermal/Power. The fetcher is injected, so these fixtures script the
// whole tree.
func TestWalkHealth(t *testing.T) {
	root := `{"Chassis":{"@odata.id":"/redfish/v1/Chassis"}}`
	coll := `{"Members":[{"@odata.id":"/redfish/v1/Chassis/1"}]}`
	chassis := `{"PowerState":"On","Status":{"Health":"OK"},
		"Thermal":{"@odata.id":"/redfish/v1/Chassis/1/Thermal"},
		"Power":{"@odata.id":"/redfish/v1/Chassis/1/Power"}}`
	thermal := `{"Fans":[
			{"Name":"Fan 1","Reading":6600,"ReadingUnits":"RPM","Status":{"Health":"OK"}},
			{"Name":"Fan 2","Reading":0,"Status":{"Health":"Warning"}}],
		"Temperatures":[
			{"Name":"Inlet","ReadingCelsius":24,"Status":{"Health":"OK"}},
			{"Name":"CPU","ReadingCelsius":88,"Status":{"Health":"Critical"}}]}`
	power := `{"PowerSupplies":[{"Name":"PSU 1","LastPowerOutputWatts":210,"Status":{"Health":"OK"}}],
		"Voltages":[{"Name":"+3.3V","ReadingVolts":3.29,"Status":{"Health":"OK"}}]}`

	tree := map[string]string{
		"/redfish/v1/":                  root,
		"/redfish/v1/Chassis":           coll,
		"/redfish/v1/Chassis/1":         chassis,
		"/redfish/v1/Chassis/1/Thermal": thermal,
		"/redfish/v1/Chassis/1/Power":   power,
	}
	get := func(url string) ([]byte, error) {
		if raw, ok := tree[url]; ok {
			return []byte(raw), nil
		}
		return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: "health", Detail: url}
	}

	got, err := walkHealth(get)
	if err != nil {
		t.Fatalf("walkHealth: %v", err)
	}
	if got.PowerState != bmc.PowerStateOn {
		t.Fatalf("power state: want on, got %q", got.PowerState)
	}
	// CPU at critical must dominate the overall verdict even though
	// everything else is fine.
	if got.Health != bmc.SensorCritical {
		t.Fatalf("overall health: want critical, got %q", got.Health)
	}
	if len(got.Sensors) != 6 {
		t.Fatalf("want 6 sensors, got %d: %+v", len(got.Sensors), got.Sensors)
	}
	// Fan 2 carries a warning with no numeric sample — the Reading 0 must
	// stay omitted at the wire layer, i.e. zero here.
	if got.Sensors[1].State != bmc.SensorWarning || got.Sensors[1].Reading != 0 {
		t.Fatalf("fan 2: %+v", got.Sensors[1])
	}
	if got.Sensors[0].Unit != "RPM" || got.Sensors[3].Unit != "Celsius" ||
		got.Sensors[4].Unit != "Watts" || got.Sensors[5].Unit != "Volts" {
		t.Fatalf("units: %+v", got.Sensors)
	}
}

func TestWalkHealthLenient(t *testing.T) {
	// One chassis unreachable, one without Thermal/Power links, one broken
	// frame, one healthy chassis with a fan missing ReadingUnits (falls
	// back to RPM) and an absent Status.Health (unknown, not warning).
	root := `{"Chassis":{"@odata.id":"/redfish/v1/Chassis"}}`
	coll := `{"Members":[
		{"@odata.id":"/redfish/v1/Chassis/dead"},
		{"@odata.id":"/redfish/v1/Chassis/bare"},
		{"@odata.id":"/redfish/v1/Chassis/broken"},
		{"@odata.id":"/redfish/v1/Chassis/ok"}]}`
	bare := `{"Id":"2"}`
	okChassis := `{"PowerState":"Off","Thermal":{"@odata.id":"/redfish/v1/Chassis/ok/Thermal"}}`
	okThermal := `{"Fans":[{"Name":"Fan 1","Reading":6000,"Status":{"Health":"OK"}}]}`

	tree := map[string]string{
		"/redfish/v1/":                   root,
		"/redfish/v1/Chassis":            coll,
		"/redfish/v1/Chassis/bare":       bare,
		"/redfish/v1/Chassis/broken":     `{"Id":`,
		"/redfish/v1/Chassis/ok":         okChassis,
		"/redfish/v1/Chassis/ok/Thermal": okThermal,
	}
	get := func(url string) ([]byte, error) {
		if raw, ok := tree[url]; ok {
			return []byte(raw), nil
		}
		return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: "health", Detail: url}
	}

	got, err := walkHealth(get)
	if err != nil {
		t.Fatalf("walkHealth: %v", err)
	}
	if got.PowerState != bmc.PowerStateOff {
		t.Fatalf("power state: want off, got %q", got.PowerState)
	}
	if len(got.Sensors) != 1 || got.Sensors[0].Unit != "RPM" {
		t.Fatalf("sensors: %+v", got.Sensors)
	}
	// One readable OK fan → overall ok; the unreadable ones must not drag
	// it to unknown.
	if got.Health != bmc.SensorOK {
		t.Fatalf("overall health: want ok, got %q", got.Health)
	}
}

func TestWalkHealthAllUnknown(t *testing.T) {
	// A chassis that reports nothing readable → overall unknown, not ok.
	root := `{"Chassis":{"@odata.id":"/redfish/v1/Chassis"}}`
	coll := `{"Members":[{"@odata.id":"/redfish/v1/Chassis/1"}]}`
	tree := map[string]string{
		"/redfish/v1/":          root,
		"/redfish/v1/Chassis":   coll,
		"/redfish/v1/Chassis/1": `{"Id":"1"}`,
	}
	get := func(url string) ([]byte, error) {
		if raw, ok := tree[url]; ok {
			return []byte(raw), nil
		}
		return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: "health", Detail: url}
	}
	got, err := walkHealth(get)
	if err != nil {
		t.Fatalf("walkHealth: %v", err)
	}
	if got.Health != bmc.SensorUnknown || len(got.Sensors) != 0 {
		t.Fatalf("want unknown with no sensors, got %q / %+v", got.Health, got.Sensors)
	}
}

func TestWalkSEL(t *testing.T) {
	root := `{"Managers":{"@odata.id":"/redfish/v1/Managers"}}`
	mgrColl := `{"Members":[{"@odata.id":"/redfish/v1/Managers/1"},{"@odata.id":"/redfish/v1/Managers/2"}]}`
	mgr1 := `{"LogServices":{"@odata.id":"/redfish/v1/Managers/1/LogServices"}}`
	lsColl := `{"Members":[{"@odata.id":"/redfish/v1/Managers/1/LogServices/SEL"}]}`
	sel := `{"Entries":{"@odata.id":"/redfish/v1/Managers/1/LogServices/SEL/Entries"}}`
	// Append-ordered oldest→newest; Severity "Debug" normalizes to unknown.
	entries := `{"Members":[
		{"@odata.id":".../Entries/1"},
		{"@odata.id":".../Entries/2"},
		{"@odata.id":".../Entries/3"},
		{"@odata.id":".../Entries/4"}]}`
	e1 := `{"Id":"1","Created":"2026-09-20T01:00:00Z","Severity":"OK","Message":"boot"}`
	e2 := `{"Id":"2","Created":"2026-09-21T01:00:00Z","Severity":"Warning","Message":" temp high "}`
	e3 := `{"Id":"3","Created":"2026-09-22T01:00:00Z","Severity":"Critical","Message":"psu lost"}`
	e4 := `{"Id":"4","Created":"2026-09-23T01:00:00Z","Severity":"Debug","Message":"oem thing"}`
	// Manager 2 has no log services link — degraded, not fatal.
	mgr2Body := `{"Id":"2"}`

	tree := map[string]string{
		"/redfish/v1/":                                   root,
		"/redfish/v1/Managers":                           mgrColl,
		"/redfish/v1/Managers/1":                         mgr1,
		"/redfish/v1/Managers/2":                         mgr2Body,
		"/redfish/v1/Managers/1/LogServices":             lsColl,
		"/redfish/v1/Managers/1/LogServices/SEL":         sel,
		"/redfish/v1/Managers/1/LogServices/SEL/Entries": entries,
		".../Entries/1":                                  e1,
		".../Entries/2":                                  e2,
		".../Entries/3":                                  e3,
		".../Entries/4":                                  e4,
	}
	get := func(url string) ([]byte, error) {
		if raw, ok := tree[url]; ok {
			return []byte(raw), nil
		}
		return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: "sel", Detail: url}
	}

	got, err := walkSEL(get)
	if err != nil {
		t.Fatalf("walkSEL: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 entries, got %d", len(got))
	}
	// newest first
	if got[0].ID != "4" || got[3].ID != "1" {
		t.Fatalf("not newest first: %+v", got)
	}
	if got[0].Severity != "unknown" {
		t.Fatalf("Debug severity must normalize to unknown, got %q", got[0].Severity)
	}
	if got[3].Severity != "ok" || got[1].Severity != "critical" || got[2].Severity != "warning" {
		t.Fatalf("severity normalization: %+v", got)
	}
	if got[2].Message != "temp high" {
		t.Fatalf("message not trimmed: %q", got[2].Message)
	}
}

func TestWalkSELOldestFirstCollection(t *testing.T) {
	// Collections arrive oldest→newest; the walk must return newest first
	// regardless (the 500-entry cap selects the collection tail before
	// bodies are fetched — covered implicitly by the small-scale order
	// check here, since SELMaxEntries is a package const).
	root := `{"Managers":{"@odata.id":"/redfish/v1/Managers"}}`
	mgrColl := `{"Members":[{"@odata.id":"/redfish/v1/Managers/1"}]}`
	mgr1 := `{"LogServices":{"@odata.id":".../LogServices"}}`
	lsColl := `{"Members":[{"@odata.id":".../LogServices/SEL"}]}`
	sel := `{"Entries":{"@odata.id":".../Entries"}}`

	var members strings.Builder
	members.WriteString(`{"Members":[`)
	for i := 1; i <= 6; i++ {
		if i > 1 {
			members.WriteString(",")
		}
		members.WriteString(`{"@odata.id":".../Entries/` + string(rune('0'+i)) + `"}`)
	}
	members.WriteString(`]}`)

	tree := map[string]string{
		"/redfish/v1/":           root,
		"/redfish/v1/Managers":   mgrColl,
		"/redfish/v1/Managers/1": mgr1,
		".../LogServices":        lsColl,
		".../LogServices/SEL":    sel,
		".../Entries":            members.String(),
	}
	for i := 1; i <= 6; i++ {
		id := string(rune('0' + i))
		tree[".../Entries/"+id] = `{"Id":"` + id + `","Created":"2026-09-2` + id + `T00:00:00Z","Severity":"OK"}`
	}
	get := func(url string) ([]byte, error) {
		if raw, ok := tree[url]; ok {
			return []byte(raw), nil
		}
		return nil, &bmc.Error{Kind: bmc.KindUnreachable, Op: "sel", Detail: url}
	}

	got, err := walkSEL(get)
	if err != nil {
		t.Fatalf("walkSEL: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("under the cap every entry comes back, got %d", len(got))
	}
	if got[0].ID != "6" {
		t.Fatalf("newest first violated: %+v", got[0])
	}
}

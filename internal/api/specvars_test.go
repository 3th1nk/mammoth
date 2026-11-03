package api

// Unit coverage for the §5.3 pipeline primitives: template resolution,
// RFC 7386 merge patch, post-substitution network validation. The full
// CreateJob round-trip is PG-gated (make test-pg); these run everywhere.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResolveTemplate(t *testing.T) {
	ctx := varContext{
		index:      2,
		machineID:  "mch_two",
		serial:     "SER222",
		bmcAddress: "10.0.0.2",
		labels:     map[string]string{"rack": "r7"},
		vars:       map[string]string{"ip": "10.0.0.22/24"},
	}
	ok := func(in, want string) {
		t.Helper()
		got, err := resolveTemplate(in, ctx)
		if err != nil || got != want {
			t.Fatalf("resolveTemplate(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	ok("node-{index}", "node-2")
	ok("{machine_id}", "mch_two")
	ok("{serial}", "SER222")
	ok("{bmc_address}", "10.0.0.2")
	ok("10.0.0.{index}/{var:ip}", "10.0.0.2/10.0.0.22/24")
	ok("{label:rack}-{index}", "r7-2")
	// literal-safety: unknown forms and shell spellings pass through
	ok("echo ${HOME} && awk '{print $1}'", "echo ${HOME} && awk '{print $1}'")
	ok("{unknown_ref}", "{unknown_ref}")
	ok("json {\"mii-monitor-interval\":100}", "json {\"mii-monitor-interval\":100}")

	if _, err := resolveTemplate("{var:missing}", ctx); err == nil || !strings.Contains(err.Error(), "{var:missing}") {
		t.Fatalf("missing binding must error naming the ref: %v", err)
	}
	if _, err := resolveTemplate("{label:nope}", ctx); err == nil || !strings.Contains(err.Error(), "{label:nope}") {
		t.Fatalf("missing label must error naming the ref: %v", err)
	}
}

func TestResolveSpecVars(t *testing.T) {
	ctx := varContext{index: 1, machineID: "mch_a", vars: map[string]string{"ip": "10.0.0.11/24"}}
	spec := map[string]any{
		"identity": map[string]any{"hostname_pattern": "node-{index}"},
		"network": []any{
			map[string]any{
				"match":     map[string]any{"name": "eno1"},
				"addresses": []any{"{var:ip}"},
			},
		},
		"count": 7,
	}
	raw, _ := json.Marshal(spec)
	out, err := resolveSpecVars(raw, ctx)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var got map[string]any
	if json.Unmarshal(out, &got) != nil {
		t.Fatalf("unmarshal resolved: %s", out)
	}
	net := got["network"].([]any)[0].(map[string]any)
	if net["addresses"].([]any)[0] != "10.0.0.11/24" {
		t.Fatalf("address not substituted: %s", out)
	}
	if got["identity"].(map[string]any)["hostname_pattern"] != "node-1" {
		t.Fatalf("hostname not expanded: %s", out)
	}
	if got["count"].(float64) != 7 {
		t.Fatalf("non-string leaves must survive untouched: %s", out)
	}

	bad, _ := json.Marshal(map[string]any{"addresses": []any{"{var:gone}"}})
	if _, err := resolveSpecVars(bad, ctx); err == nil || !strings.Contains(err.Error(), "{var:gone}") {
		t.Fatalf("unresolved var in tree must error: %v", err)
	}
}

func TestMergePatch(t *testing.T) {
	base := json.RawMessage(`{"image":{"distro":"rocky9"},"storage":{"disks":[{"keep":"","wipe":true}]},"access":{"ssh_keys":["a"]}}`)

	patch := json.RawMessage(`{"storage":{"disks":[{"keep":"","wipe":true,"partitions":[{"size":"rest"}]}]},"access":null}`)
	out, err := mergePatch(base, patch)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var got map[string]any
	if json.Unmarshal(out, &got) != nil {
		t.Fatalf("unmarshal: %s", out)
	}
	// nested object merge keeps siblings; null deletes the key
	disk := got["storage"].(map[string]any)["disks"].([]any)[0].(map[string]any)
	if disk["wipe"] != true || len(disk["partitions"].([]any)) != 1 {
		t.Fatalf("deep merge wrong: %s", out)
	}
	if _, has := got["access"]; has {
		t.Fatalf("null must delete the key: %s", out)
	}
	if got["image"].(map[string]any)["distro"] != "rocky9" {
		t.Fatalf("untouched sibling lost: %s", out)
	}

	// non-object patch replaces wholesale (RFC 7386 §2)
	out, _ = mergePatch(base, json.RawMessage(`[]`))
	if string(out) != `[]` {
		t.Fatalf("array patch must replace: %s", out)
	}
	// no patch / null patch → base unchanged
	out, _ = mergePatch(base, nil)
	if string(out) != string(base) {
		t.Fatalf("nil patch must return base: %s", out)
	}
}

func TestValidateResolvedNetwork(t *testing.T) {
	valid := json.RawMessage(`{"network":[
		{"match":{"mac":"aa:bb:cc:dd:ee:01"},"addresses":["10.0.0.11/24"],"routes":[{"to":"default","via":"10.0.0.1"}],"nameservers":{"addresses":["10.0.0.53","fd00::53"]}},
		{"bond":{"mode":"active-backup"},"addresses":["10.1.0.2/24"]},
		{"vlan":{"id":100,"link":"bond0"},"addresses":["10.2.0.2/24"]}
	]}`)
	if err := validateResolvedNetwork(valid); err != nil {
		t.Fatalf("valid network rejected: %v", err)
	}

	fail := func(raw string, wantSub string) {
		t.Helper()
		err := validateResolvedNetwork(json.RawMessage(raw))
		if err == nil || !strings.Contains(err.Error(), wantSub) {
			t.Fatalf("expected rejection naming %q, got: %v", wantSub, err)
		}
	}
	fail(`{"network":[{"match":{"mac":"aa"},"addresses":["10.0.0.999/24"]}]}`, "10.0.0.999")
	// bare IP (no prefix): the render layer parses addresses with
	// netip.ParsePrefix — reject at submission with the actionable form
	fail(`{"network":[{"match":{"mac":"aa"},"addresses":["192.168.1.221"]}]}`, "CIDR form")
	fail(`{"network":[{"match":{"mac":"aa"},"addresses":["not-an-ip"]}]}`, "not-an-ip")
	fail(`{"network":[{"match":{"mac":"aa"},"nameservers":{"addresses":["1.2.3.4.5"]}}]}`, "nameserver")
	fail(`{"network":[{"match":{"mac":"aa"},"routes":[{"to":"default","via":"gg::1"}]}]}`, "route via")
	fail(`{"network":[{"vlan":{"id":0,"link":"bond0"}}]}`, "vlan id")
}

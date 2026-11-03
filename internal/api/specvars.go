package api

// Per-machine variable binding and template substitution
// (docs/04-install-spec.md §5.3): the submitted spec is a TEMPLATE — any
// string value may carry references that resolve against the target's
// bindings (targets[].vars) and machine context. Resolution happens at
// submission, per machine, BEFORE per-task validation, so a spec that
// resolves here installs there and a missing binding rejects the request
// (422 naming machine + variable) instead of failing mid-install.
//
// Reference syntax and the literal-safe rule:
//   {var:name}      → targets[].vars[name]         (missing → 422)
//   {index}         → 1-based position in targets
//   {machine_id}    → the target machine's id
//   {serial}        → the machine's inventoried serial ("" when unknown)
//   {bmc_address}   → the machine's BMC address
//   {label:key}     → machine label value           (missing → 422)
// Any other `{...}` sequence stays literal — shell `${...}`, awk snippets
// and JSON examples in script strings pass through untouched. base64 script
// bodies are opaque and never templated.

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
)

// varContext is everything a reference can resolve against for one machine.
type varContext struct {
	index      int // 1-based position in the targets array
	machineID  string
	serial     string
	bmcAddress string
	labels     map[string]string
	vars       map[string]string
}

// resolveTemplate expands one string's references. Unknown reference forms
// are kept literal; a KNOWN form that cannot resolve is a request-level
// error — the caller names the machine (SCHEMA_UNRESOLVED_VAR).
func resolveTemplate(s string, ctx varContext) (string, error) {
	var unresolved string
	out := expandRefs(s, func(ref string) (string, bool) {
		switch {
		case ref == "index":
			return fmt.Sprint(ctx.index), true
		case ref == "machine_id":
			return ctx.machineID, true
		case ref == "serial":
			return ctx.serial, true
		case ref == "bmc_address":
			return ctx.bmcAddress, true
		case strings.HasPrefix(ref, "var:"):
			name := strings.TrimPrefix(ref, "var:")
			if v, ok := ctx.vars[name]; ok {
				return v, true
			}
			unresolved = "{" + ref + "}"
			return "", false
		case strings.HasPrefix(ref, "label:"):
			key := strings.TrimPrefix(ref, "label:")
			if v, ok := ctx.labels[key]; ok {
				return v, true
			}
			unresolved = "{" + ref + "}"
			return "", false
		default:
			return "{" + ref + "}", true // not a recognized form — literal
		}
	})
	if unresolved != "" {
		return "", fmt.Errorf("unresolved reference %s", unresolved)
	}
	return out, nil
}

// expandRefs scans s for `{...}` tokens and rebuilds the string through
// resolve. Only balanced, brace-delimited tokens are candidates.
func expandRefs(s string, resolve func(ref string) (string, bool)) string {
	var b strings.Builder
	for {
		open := strings.IndexByte(s, '{')
		if open < 0 {
			b.WriteString(s)
			return b.String()
		}
		close_ := strings.IndexByte(s[open:], '}')
		if close_ < 0 {
			b.WriteString(s)
			return b.String()
		}
		close_ += open
		b.WriteString(s[:open])
		replaced, ok := resolve(s[open+1 : close_])
		if !ok {
			b.WriteString(s[open : close_+1]) // unresolved — keep the raw token
		} else {
			b.WriteString(replaced)
		}
		s = s[close_+1:]
	}
}

// resolveSpecVars walks a spec JSON tree and expands every string value.
// Keys are never templated; object/array structure is preserved.
func resolveSpecVars(raw json.RawMessage, ctx varContext) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("spec is not valid JSON: %w", err)
	}
	resolved, err := resolveValue(v, ctx)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(resolved)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func resolveValue(v any, ctx varContext) (any, error) {
	switch t := v.(type) {
	case string:
		return resolveTemplate(t, ctx)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			r, err := resolveValue(e, ctx)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			r, err := resolveValue(e, ctx)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	default:
		return v, nil
	}
}

// mergePatch applies an RFC 7386 JSON Merge Patch over base. A nil patch
// returns base unchanged; `null` members delete the key from base objects.
func mergePatch(base, patch json.RawMessage) (json.RawMessage, error) {
	if len(patch) == 0 || string(patch) == "null" {
		return base, nil
	}
	var b, p any
	if len(base) > 0 {
		if err := json.Unmarshal(base, &b); err != nil {
			return nil, fmt.Errorf("base spec is not valid JSON: %w", err)
		}
	}
	if err := json.Unmarshal(patch, &p); err != nil {
		return nil, fmt.Errorf("spec_patch is not valid JSON: %w", err)
	}
	return json.RawMessage(mustJSON(mergePatchValue(b, p))), nil
}

func mergePatchValue(base, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch // non-object patch replaces wholesale (RFC 7386 §2)
	}
	bm, ok := base.(map[string]any)
	if !ok {
		bm = map[string]any{}
	}
	out := make(map[string]any, len(bm)+len(pm))
	for k, v := range bm {
		out[k] = v
	}
	for k, v := range pm {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = mergePatchValue(out[k], v)
	}
	return out
}

// validateResolvedNetwork is the post-substitution network sanity gate:
// binding values land verbatim in netplan/kickstart/preseed address fields,
// so a typo must reject at submission (SCHEMA_INVALID_ADDRESS), not surface
// as a machine that boots into the installer with no network. Runs on the
// RESOLVED spec — addresses may have come from {var:*}.
func validateResolvedNetwork(raw json.RawMessage) error {
	var spec struct {
		Network []netEntryView `json:"network"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil // structural errors surface via the schema validation path
	}
	for i, e := range spec.Network {
		if e.VLAN != nil && e.VLAN.ID <= 0 {
			return verr("SCHEMA_INVALID_ADDRESS", "network[%d]: vlan id must be a positive 802.1Q tag", i)
		}
		for _, a := range e.Addresses {
			if !validIPorCIDR(a) {
				return verr("SCHEMA_INVALID_ADDRESS", "%s: address %q is not a valid IP or CIDR", e.name(i), a)
			}
		}
		for _, d := range e.Nameservers.Addresses {
			if net.ParseIP(strings.Trim(d, "[]")) == nil {
				return verr("SCHEMA_INVALID_ADDRESS", "%s: nameserver %q is not a valid IP address", e.name(i), d)
			}
		}
		for _, r := range e.Routes {
			if r.Via != "" && net.ParseIP(strings.Trim(r.Via, "[]")) == nil {
				return verr("SCHEMA_INVALID_ADDRESS", "%s: route via %q is not a valid IP address", e.name(i), r.Via)
			}
		}
	}
	return nil
}

// netEntryView mirrors the contract's network entries for validation only —
// the render layer's richer parse (internal/provision install.go networkView)
// stays the single consumer at execution.
type netEntryView struct {
	Match *struct {
		MAC string `json:"mac"`
	} `json:"match"`
	Bond *struct {
		Mode string `json:"mode"`
	} `json:"bond"`
	VLAN *struct {
		ID   int    `json:"id"`
		Link string `json:"link"`
	} `json:"vlan"`
	Addresses   []string `json:"addresses"`
	Nameservers struct {
		Addresses []string `json:"addresses"`
	} `json:"nameservers"`
	Routes []struct {
		To  string `json:"to"`
		Via string `json:"via"`
	} `json:"routes"`
}

// names the entry in error messages: the field the operator can find in
// their submission — bond/vlan/mac position.
func (e netEntryView) name(i int) string {
	switch {
	case e.Bond != nil:
		return fmt.Sprintf("network[%d] (bond)", i)
	case e.VLAN != nil:
		return fmt.Sprintf("network[%d] (vlan %d)", i, e.VLAN.ID)
	case e.Match != nil && e.Match.MAC != "":
		return fmt.Sprintf("network[%d] (mac %s)", i, e.Match.MAC)
	default:
		return fmt.Sprintf("network[%d]", i)
	}
}

// validIPorCIDR accepts a bare IP or an address with a prefix length —
// cloud-init v2 accepts both spellings and so does the render layer.
func validIPorCIDR(s string) bool {
	if net.ParseIP(strings.Trim(s, "[]")) != nil {
		return true
	}
	ip, _, err := net.ParseCIDR(s)
	return err == nil && ip != nil
}

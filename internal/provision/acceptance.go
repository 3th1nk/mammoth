package provision

// The post-install acceptance report (docs/06-install-pipeline.md §5,
// docs/09-roadmap.md 下一阶段 12): the freshly installed system's collected
// facts held against the declared intent — the evidence face the six-stage
// pipeline ends with. Evidence, never a gate: a mismatched check degrades
// the verdict (surfaced as an event and persisted for the read face), the
// task outcome stays verify_ready's business (its bond/LVM gates already
// hard-fail installs). The report rides the SAME in-band collection
// verify_ready already ran — no second SSH round trip.

import (
	"github.com/3th1nk/mammoth/internal/inventory/inbandssh"
	"github.com/3th1nk/mammoth/internal/store"
)

// buildAcceptanceReport compares the collected facts against the declared
// intent and returns the persisted report. Checks fire only where intent
// exists — an undeclared expectation produces no check (no invented
// evidence). Facts unavailable reads "degraded", never "fail".
func buildAcceptanceReport(res *inbandssh.Result, ictx *installTaskContext) *store.AcceptanceReport {
	checks := []store.AcceptanceCheck{}

	// system_facts: the ===SYS=== capture itself (the collector section is
	// optional — recorded fixtures predate it).
	if res.System == nil {
		checks = append(checks, store.AcceptanceCheck{
			Name: "system_facts", Status: "unavailable",
			Detail: "collector output predates the ===SYS=== section",
		})
	} else {
		checks = append(checks, store.AcceptanceCheck{
			Name: "system_facts", Status: "ok",
			Detail: "kernel=" + res.System.Kernel + " os=" + res.System.OS,
		})
	}

	// hostname: the resolved identity intent (pattern-expanded at submit)
	// against the live system's own claim.
	if ictx.Hostname != "" {
		if res.System == nil || res.System.Hostname == "" {
			checks = append(checks, store.AcceptanceCheck{
				Name: "hostname", Status: "unavailable",
				Detail: "system facts missing the hostname fact",
			})
		} else if res.System.Hostname == ictx.Hostname {
			checks = append(checks, store.AcceptanceCheck{
				Name: "hostname", Status: "ok", Detail: ictx.Hostname,
			})
		} else {
			checks = append(checks, store.AcceptanceCheck{
				Name: "hostname", Status: "mismatch",
				Detail: "declared " + ictx.Hostname + ", live " + res.System.Hostname,
			})
		}
	}

	// storage_mounts: every declared filesystem mount must exist in the
	// live layout (mountpoints as the installed system sees them). The
	// full layout diff is the snapshot's business — this check is the
	// "the declared mounts came up" floor.
	declared := declaredMounts(ictx)
	if len(declared) > 0 {
		if live := liveMounts(res); len(live) == 0 {
			checks = append(checks, store.AcceptanceCheck{
				Name: "storage_mounts", Status: "unavailable",
				Detail: "layout carries no mount facts",
			})
		} else {
			var missing []string
			for _, m := range declared {
				if !live[m] {
					missing = append(missing, m)
				}
			}
			if len(missing) == 0 {
				checks = append(checks, store.AcceptanceCheck{
					Name: "storage_mounts", Status: "ok",
					Detail: joinStrings(declared, " "),
				})
			} else {
				checks = append(checks, store.AcceptanceCheck{
					Name: "storage_mounts", Status: "mismatch",
					Detail: "not mounted: " + joinStrings(missing, " "),
				})
			}
		}
	}

	// boot_cmdline: the boot chain's own words — present means the
	// installed bootloader carried the engine's boot line through
	// (ubuntu26 confirmation-gate forensics read exactly this).
	if res.System != nil {
		if res.System.Cmdline != "" {
			checks = append(checks, store.AcceptanceCheck{
				Name: "boot_cmdline", Status: "ok",
			})
		} else {
			checks = append(checks, store.AcceptanceCheck{
				Name: "boot_cmdline", Status: "unavailable",
				Detail: "/proc/cmdline unreadable in the collected facts",
			})
		}
	}

	verdict := "pass"
	for _, c := range checks {
		if c.Status == "mismatch" {
			verdict = "fail"
			break
		}
	}
	if verdict != "fail" {
		for _, c := range checks {
			if c.Status == "unavailable" {
				verdict = "degraded"
				break
			}
		}
	}
	return &store.AcceptanceReport{Verdict: verdict, Checks: checks}
}

// declaredMounts collects the resolved spec's non-empty filesystem mounts
// (the dd pathway declares none — its image owns the layout).
func declaredMounts(ictx *installTaskContext) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range ictx.Resolved.Disks {
		for _, p := range d.Partitions {
			if p.Mount == "" || seen[p.Mount] {
				continue
			}
			seen[p.Mount] = true
			out = append(out, p.Mount)
		}
	}
	return out
}

// liveMounts collects the live layout's mountpoints (empty set = the
// collection carried no mount facts — degrade, not fail).
func liveMounts(res *inbandssh.Result) map[string]bool {
	out := map[string]bool{}
	for _, d := range res.Layout.Disks {
		for _, p := range d.Partitions {
			if p.Mountpoint != "" {
				out[p.Mountpoint] = true
			}
		}
	}
	return out
}

func joinStrings(items []string, sep string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

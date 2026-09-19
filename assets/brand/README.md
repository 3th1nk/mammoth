# Brand assets

This directory is the **canonical, complete brand source pack** for the
mammoth gopher — the upstream design workspace is no longer maintained;
everything lives here from now on. The visual spec sheet is [SPEC.md](SPEC.md)
and [brand-board.png](brand-board.png).

| File | Use |
|------|-----|
| `logo.svg` | **Primary mark** — light backgrounds (README, docs, slides) |
| `logo-dark.svg` | Dark backgrounds (GitHub dark mode via `prefers-color-scheme`) |
| `logo-mono.svg` | Single-color surfaces — print, engraving, stencil |
| `logo-1024.png` / `logo-512.png` / `logo-64.png` | Raster exports (social preview upload, favicon source, avatar) |
| `brand-board.png` | Brand spec board v1.0: variants, minimum sizes, color system, lockup rules |
| `SPEC.md` | Design specification (verbatim from the original design pack) |
| `ref-gopher01c.svg` | Reference: the original Go gopher vector the mark derives from |

Color system: Go cyan `#00ADD8`, shadow `#0089AC`, ivory `#FFF7E8`, ink `#0E2A3A`.
Wordmark: "mammoth", Inter Black, lowercase. Primary tagline: **"The mammoth
task, tamed."** (the zh tagline is the product's own promise: "给它一个带外地址和一份凭证
——或者让机器自己找上门——还你一台能跑起来的机器"); secondary: "Evolutionary Go Engineering".

## Brand story — the heartbeat under the ice

English calls a colossal chore **a mammoth task** — and provisioning a fleet
of bare-metal servers is exactly that chore: burned USB sticks, KVM sessions
at 3 a.m., kickstart debugging by hand.

A server without an OS is a mammoth frozen in permafrost. But there is still
a heartbeat under the ice: the BMC, the out-of-band chip that keeps pulsing
even when the machine is "dead". Mammoth follows that heartbeat to find the
machine, inventories its skeleton, listens to your declared intent, then
carries the heavy lifting alone — repacking ISOs, mounting virtual media, the
DHCP/PXE dance, three installer dialects, verification and callback. It moves
the way its ice-age namesake does: unhurried, stubborn, reliable — interrupted
tasks are picked up and retried, and every step leaves a footprint.

Give it an out-of-band address and a credential — or let the machine
introduce itself — and get back a machine that runs.

**The mammoth task, tamed.**

The mark itself tells the story in one picture: a gopher in mammoth fur — a
small single binary (distroless, self-contained) doing mammoth-sized work.

### Metaphor map

Keep metaphors out of user-facing APIs; at most one or two may surface as
internal codenames. The map exists to keep docs, talks and naming consistent.

| Metaphor | Product fact |
|----------|--------------|
| frozen mammoth | a bare-metal server with no OS |
| heartbeat under the ice | the BMC out-of-band channel (Redfish, IPMI fallback) |
| awakening | install / reinstall the OS |
| skeleton inventory | hardware & partition-layout inventory |
| your wish | declarative Install Spec |
| trunk sniffing | zero-registration (PXE probe tree → pending → claim) |
| carrying the load | ISO repacking, virtual media, DHCP/PXE, installer dialects |
| unhurried gait | task queue + heartbeat + reaper + at-least-once delivery |
| footprints | structured logs, SSE/Webhook events |
| the herd | batch provisioning |

## Provenance & license

- The mark is a derivative of the **original Go gopher by Renée French**
  (CC BY 3.0), adapted into a mammoth form and released under the same
  license. Attribution is required and included here and in the root README.
- `ref-gopher01c.svg` originates from keygx/Go-gopher-Vector (CC BY 3.0).

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/brand/logo-dark.svg">
    <img src="assets/brand/logo.svg" alt="mammoth logo" width="140">
  </picture>
</p>

# Mammoth

[![ci](https://github.com/3th1nk/mammoth/actions/workflows/ci.yml/badge.svg)](https://github.com/3th1nk/mammoth/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/3th1nk/mammoth)](https://github.com/3th1nk/mammoth/releases)
[![license](https://img.shields.io/github/license/3th1nk/mammoth)](LICENSE)

> A self-contained bare-metal provisioning engine.
> Give it an out-of-band address and a credential — or let the machine introduce itself — and get back a machine that runs.

[**中文**](README.zh-CN.md)

Mammoth takes over machines through their out-of-band controllers (BMC),
inventories hardware and disk layout, and installs/reinstalls operating
systems from declarative intent — while exposing generic power / boot-device /
virtual-media operations as a first-class API. **Backend only, API-first;
no built-in UI.**

- Design documents: [docs/README.md](docs/README.md)
- API contract (single source of truth): [api/openapi.yaml](api/openapi.yaml)
- Status & per-version changes: see [Releases](https://github.com/3th1nk/mammoth/releases)
  and the [roadmap](docs/09-roadmap.md).

## Why the name Mammoth

English calls a colossal chore **a mammoth task** — and provisioning a fleet
of bare-metal servers is exactly that chore. A server with no OS is a mammoth
frozen in permafrost, but there is still a heartbeat under the ice: the BMC,
an out-of-band chip that keeps pulsing even when the machine is "dead".
Mammoth follows that heartbeat, inventories the skeleton, listens to your
declared intent, then carries the heavy lifting alone — repacking ISOs,
virtual media, the DHCP/PXE dance, four installer dialects. Send in an
address and a credential; get back a machine that runs.

**The mammoth task, tamed.**

The gopher in mammoth fur at the top says the same thing: a small binary
(single, self-contained) doing mammoth-sized work.

## Try it in 5 minutes — the sandbox

No BMC hardware, no second machine: `mammoth sandbox up` turns a laptop into
a small datacenter — virtual machines with simulated BMCs get registered,
inventoried and provisioned with real alpine systems, end to end through the
default virtual-media carrier, then second-booted for SSH.

Requires: Go ≥ 1.26, Docker, QEMU, xorriso, curl.

```bash
brew install qemu xorriso     # linux: sudo apt-get install qemu-system-x86 qemu-utils xorriso
go build -o bin/mammoth ./cmd/mammoth
bin/mammoth sandbox up        # --nodes N (1–5, default 3)
```

Docker supplies only PostgreSQL; guests are isolated slirp NATs — no bridges,
no privileged containers, no root. Budget roughly 2 GB RAM and one CPU core
per guest (3 guests ≈ 6 GB; without KVM a full install runs a few minutes
per guest). When the install job goes green:

```bash
ssh -i ~/.mammoth-sandbox/id_ed25519 -p 2222 root@127.0.0.1   # sandbox-node-1
bin/mammoth sandbox status    # what is running, how far the install got
bin/mammoth sandbox down      # stop everything; ISO cache and logs are kept
```

Port collisions are handled: override `MAMMOTH_SANDBOX_HTTP_PORT`,
`MAMMOTH_SANDBOX_DB_PORT`, `MAMMOTH_SANDBOX_SSH_PORT`. The sandbox machines
are fake-BMC demos — real fleets speak Redfish/IPMI through the same API and
install specs.

## Drive it from your AI — the MCP server

`mammoth mcp serve` exposes the same API as MCP tools over stdio: register
machines, drive discovery, submit declarative installs, track jobs to
completion, read task logs. All tools are read-only except `install_submit`
(which wipes and reinstalls the targets) and `job_cancel`.

```json
{
  "mcpServers": {
    "mammoth": {
      "command": "/usr/local/bin/mammoth",
      "args": ["mcp", "serve"],
      "env": {
        "MAMMOTH_API_URL": "http://127.0.0.1:8080",
        "MAMMOTH_API_TOKEN": "<token>"
      }
    }
  }
}
```

Point Claude Desktop or Cursor at the sandbox (above) and just ask:
*"machines_list, then install Rocky 9 from this ISO on every ready machine
and wait for the job"* — the mammoth task, tamed by one sentence.

## At a glance

**One pipeline, three inventory paths, two boot carriers, four installer
dialects.**

```mermaid
flowchart TD
    subgraph ON["1 · Onboarding"]
        reg["register a machine:<br/>BMC address + credential<br/>(Redfish, IPMI fallback)"]
        zr["zero-registration:<br/>unknown machine PXE-boots<br/>the shared probe tree<br/>→ pending_machines → claim"]
    end
    subgraph INV["2 · Inventory / probe"]
        rf["redfish (out-of-band)"]
        ram["ramdisk probe (alpine):<br/>PXE or virtual media"]
        ish["inband_ssh probe"]
    end
    subgraph INS["3 · Install — one declarative Install Spec"]
        vm["virtual_media (default):<br/>BMC mounts a rebuilt boot ISO<br/>(NFS/HTTP media repo)"]
        pxe["pxe (opt-in):<br/>shim → grubnet → kernel<br/>DHCP/proxyDHCP + TFTP + HTTP"]
    end
    subgraph DIA["4 · Installer dialects"]
        ks["kickstart<br/>rocky · centos · kylin · UOS"]
        ai["autoinstall<br/>ubuntu 22.04 / 24.04 / 26.04"]
        ps["preseed<br/>debian 12 / 13"]
        wu["unattend<br/>windows 2019 / 2022 / 2025 (UEFI)<br/>boots differently ⤓ windows pathways"]
    end
    ver["5 · verify: completion report +<br/>in-band SSH probe (installer-aware)<br/>+ post-install layout snapshot"]
    reg --> rf
    zr --> ram
    rf --> vm
    ram --> vm
    vm --> ks
    vm --> ai
    vm --> ps
    pxe --> ks
    pxe --> ai
    pxe --> ps
    ks --> ver
    ai --> ver
    ps --> ver
    wu --> ver
```

### Windows pathways — windows boots differently

Both windows pathways avoid the standard kernel-PXE shape: setup rides
**wimboot** (bootmgfw/BCD/boot.sdi/boot.wim fed over HTTP — plain iPXE, no
shim/grub), and the agent pathway is **two-boot** (the second boot re-arms
the same wimboot carrier so `bcdboot` — not Linux — writes the BCD).

```mermaid
flowchart TD
    subgraph WS["windows 2019 / 2022 / 2025 · boot.installer"]
        st["**setup** (default)<br/>PXE: plain iPXE → wimboot<br/>(bootmgfw · BCD · boot.sdi · boot.wim)<br/>install source: deployment SMB export<br/>vMedia: rebuilt ISO + autounattend"]
        a1["**agent · boot one**<br/>alpine agent → wimlib applies<br/>install.wim onto NTFS + in-wim<br/>injection · ESP left empty"]
        a2["**agent · boot two**<br/>re-armed wimboot WinPE →<br/>bcdboot writes the BCD natively"]
    end
    st --> fbt["first boot:<br/>specialize/OOBE → completion callback"]
    a1 -->|"applied → re-arm PXE"| a2 --> fbt
```

### Distro adaptation matrix — which image, which carrier

| Distro | Dialect | virtual_media image | PXE image | PXE install source | Real hardware |
|---|---|---|---|---|---|
| rocky 9 | kickstart | minimal / DVD ISO (repacked) | same ISO (boot files extracted) | HTTP pool or NFS ISO | ✅ both |
| rocky 10 | kickstart | DVD ISO, UEFI-only layout | same | same | ✅ real closed-loop (2288H, UEFI-only) |
| centos 7 | kickstart | minimal ISO | same ISO | same | ✅ vMedia |
| kylin V10 / V11 | kickstart | DVD ISO | same ISO | same | ✅ V11 closed-loop · V10 limited (static-net NM) |
| UOS | kickstart | DVD ISO | same ISO | NFS ISO | ✅ both |
| ubuntu 22.04 / 24.04 / 26.04 | autoinstall | **live-server** ISO (casper, repacked) | **live-server** ISO (squashfs over NFS) | unpacked ISO tree over NFS | ✅ both |
| debian 12 / 13 | preseed | **netinst** ISO (repacked) | **netinst** ISO (signed HTTP pool) **+ official netboot.tar.gz** + staged udebs | HTTP pool (checksum-complete, by-hash backfilled) | ✅ both |
| windows 2019 / 2022 / 2025 | unattend | official media repacked (root autounattend + SetupComplete wimlib injection, UDF bridge) | setup: wimboot-over-PXE (SMB install source) · agent: two-stage apply (HTTP win tree, no SMB) | — | ✅ real: six-stage green — 2019 (setup + agent apply) · 2022/2025 (setup, updated Oct 2026 media) |

Rule of thumb: **netinst / minimal** = small installer with its own package
pool (PXE-friendly); **DVD** = fully offline pool; **live-server** = ubuntu's
installer carrier (casper); **live desktop** = unsupported (no installer
inside). The virtual_media carrier always repacks the official ISO with the
answer files baked in — the PXE carrier extracts boot files and serves the
ISO content as a package source.

### Regression baseline — mainstream server images

| Family | Baseline image (latest point release) | Carrier coverage |
|---|---|---|
| RHEL-like | Rocky 9.x minimal ISO | virtual_media ✅ · PXE ✅ |
| Ubuntu-like | Ubuntu 22.04.5 & 24.04.x & 26.04.1 live-server ISO | virtual_media ✅ · PXE ✅ |
| Debian-like | Debian 12 / 13 netinst ISO | virtual_media ✅ · PXE ✅ |
| Windows-like | Windows Server 2019 / 2022 / 2025 (zh-CN, updated Oct 2026 snapshots verified) | PXE (wimboot + SMB source) ✅ real, six stages green — 2019/2022/2025 · virtual_media 🔧 (blocked by iBMC 6.41 firmware, UEFI-only, Standard Core) |
| Extension | Rocky 10 (UEFI-only) · CentOS 7 (legacy) · Kylin V10/V11 · UOS | per-demand |

Every regression run: six-stage pipeline green → unattended first boot →
SSH probe with the provisioned key. See
[docs/runbooks/test-baselines.md](docs/runbooks/test-baselines.md).

### Cross-subnet deployments — one engine, many VLAN agents

Production rooms are cross-subnet: the engine lives in the management
network, and every provisioning VLAN needs a PXE first hop on its own wire.
`mammoth pxe-proxy` is that first hop as a single static binary — proxyDHCP
answers, TFTP for the boot chain, PXE sightings reported back — configured
with just the engine URL. It replaces a hand-tuned per-VLAN dnsmasq
(config drift and kit-sync copy-paste were the failure modes it exists to
remove).

Two addressing modes cover every room. **No site DHCP** → POOL mode: the
engine's built-in DHCP hands out addresses from a declared pool, only to
machines with an armed install. **Site DHCP present** → PROXY mode: the
site keeps assigning addresses, and `pxe-proxy` (or an external dnsmasq)
adds only the PXE boot options on the agent's L2. Cross-VLAN rooms deploy
a DHCP relay (ip helper) and route TFTP/HTTP to the engine — details in
[docs/operations.md](docs/operations.md) §pxe-proxy.

```mermaid
flowchart TD
    eng["mammoth engine<br/>(management network)"]
    subgraph V1["provisioning VLAN 1"]
        a1["pxe-proxy<br/>proxyDHCP + TFTP<br/>(sightings → engine)"]
        m1["bare-metal machines"]
    end
    subgraph V2["provisioning VLAN N"]
        aN["pxe-proxy"]
        mN["bare-metal machines"]
    end
    eng -->|kit sync + observe POST| a1
    eng --> aN
    a1 --> m1
    aN --> mN
```

Setup is one binary plus one line per VLAN agent
(`mammoth pxe-proxy --base-url …`); the kit syncs itself and sightings feed
the engine's pending_machines ledger. Firewall: UDP 67/69/4011 open on the
agent, TCP <engine>:8080 reachable from the VLAN. Full runbook — including
the Secure Boot and address-squatting notes — lives in
[docs/operations.md](docs/operations.md) §pxe-proxy.

## Quick start (all-in-one)

Requirements: Go ≥ 1.26, Docker (for PostgreSQL).

```bash
docker run -d --name mammoth-pg -e POSTGRES_USER=mammoth -e POSTGRES_PASSWORD=mammoth \
  -e POSTGRES_DB=mammoth -p 5432:5432 postgres:16-alpine

go build -o bin/mammoth ./cmd/mammoth

MAMMOTH_DATABASE_URL='postgres://mammoth:mammoth@localhost:5432/mammoth?sslmode=disable' \
MAMMOTH_API_TOKEN=devtoken \
MAMMOTH_MASTER_KEY=$(openssl rand -base64 32) \
bin/mammoth serve --mode=all
```

Configuration is 12-factor env (`MAMMOTH_*`). For bare-binary deployments a
dotenv file works too — existing environment variables win over file entries,
and invalid values fail startup instead of silently defaulting. A full
worked example ([deploy/mammoth.env.example](deploy/mammoth.env.example)) is
derived from a real deployment — copy it, edit the generated keys, addresses
and paths:

```bash
cp deploy/mammoth.env.example /etc/mammoth.env   # edit keys/IPs/paths
bin/mammoth serve --env-file /etc/mammoth.env    # or MAMMOTH_ENV_FILE=...
```

Try it:

```bash
TOKEN='Authorization: Bearer devtoken'
# 1. a credential (write-only; never echoed back)
curl -s -X POST -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"bmc","name":"demo","secret":{"username":"admin","password":"..."}}' \
  localhost:8080/api/v1/credentials
# 2. register a machine (protocol=fake → built-in BMC simulator)
curl -s -X POST -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{"bmc":{"address":"fake://n1","protocol":"fake","credential_id":"cred_..."}}' \
  localhost:8080/api/v1/machines
# 3. inspect the probe results (registration auto-discovers; hardware is
#    spec-level, /layout is the partition snapshot and needs an in-band path)
curl -s -H "$TOKEN" localhost:8080/api/v1/machines/mch_...           # hardware / firmware / power_state
curl -s -H "$TOKEN" localhost:8080/api/v1/machines/mch_.../layout    # latest layout snapshot (mind captured_at)
# snapshots state facts at capture time — when the probe→install gap grows
# long, refresh before planning:
curl -s -X POST -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"discover"}' localhost:8080/api/v1/machines/mch_.../actions
# 4. dry-run the install plan (read-only resolution: which disk the
#    selector picks, whether keep hits the snapshot — no boot burned)
curl -s -X POST -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{"spec":{"image":{"distro":"rocky9"},"storage":{"disks":[{"select":{"match":{"size":"largest"}},"wipe":true}]}}}' \
  localhost:8080/api/v1/machines/mch_.../install-plan

# 5. batch install (the complete Install Spec: one intent, rendered into
#    the distro's dialect)
curl -s -X POST -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{
    "type": "install",
    "targets": {"machine_ids": ["mch_a", "mch_b"]},
    "spec": {
      "image":  {"source": "https://mirror.example/rocky9.iso", "distro": "rocky9"},
      "storage": {"disks": [{
          "select": {"match": {"type": "nvme", "size": "largest"}},
          "wipe": true,
          "partitions": [
            {"size": "512M", "fs": "vfat", "mount": "/boot/efi", "flags": ["esp"]},
            {"size": "1G",   "fs": "xfs",  "mount": "/boot"},
            {"size": "rest", "fs": "xfs",  "mount": "/"}]}]},
      "network": [{
          "match": {"mac": "aa:bb:cc:dd:ee:01"}, "set_name": "eno1",
          "addresses": ["172.16.1.11/24"],
          "routes": [{"to": "default", "via": "172.16.1.1"}],
          "nameservers": {"addresses": ["10.0.0.53"]}}],
      "identity": {"hostname_pattern": "node-{index}"},
      "access":   {"ssh_keys": ["ssh-ed25519 AAA you@host"]},
      "scripts":  [{"stage": "post_install", "content_base64": "ZWNobyBkb25lCg=="}],
      "boot":     {"strategy": "virtual_media"}
    },
    "policy": {"concurrency": 2, "on_task_failure": "continue"}
  }'
# → 202 + job_id;both machines install concurrently, one failure does not
#   block the batch
# → root password defaults to a per-task random, delivered once via the
#   task.root_password event
# → keep semantics: disks[].keep: disk|partitions + preserve (by snapshot
#   partition number; support matrix in docs/06 §5, install-plan pre-checks)
# → bond/vlan, software+hardware RAID, the pxe carrier: full surface in docs/04

# 6. generic out-of-band actions (power/media/boot-device — first-class API
#    decoupled from installs)
curl -s -X POST -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"power_on"}' localhost:8080/api/v1/machines/mch_.../actions

# 7. watch the job (event stream: SSE /api/v1/events; full scripted
#    acceptance: scripts/acceptance.py — it plays the fake machines, fetching
#    answer files and reporting completion)
curl -s -H "$TOKEN" localhost:8080/api/v1/jobs/job_...
```

Run the scripted acceptance flow (includes a SIGKILL → reaper → retry proof):

```bash
MAMMOTH_FAKE_BMC_DELAY=8s bin/mammoth serve --mode=all &   # slow fake BMC
python3 scripts/acceptance.py --api http://localhost:8080 --token devtoken \
  --dsn 'postgres://mammoth:mammoth@localhost:5432/mammoth?sslmode=disable'
```

## Architecture in one screen

```
client ──▶ api (control plane, stateless)      ──┐
            runner (executes tasks via BMC)    ──┼──▶ PostgreSQL (state + table queue)
            builder (media assembly)           ──┤
            prober (in-band probes)            ──┘
```

- Single binary, multiple facets: `serve --mode=all|api|runner|builder|prober`
- At-least-once task queue with visibility timeouts (`SKIP LOCKED` on PG);
  state transitions and queue ops share one transaction domain
- Every task has a heartbeat; a reaper marks lost tasks `interrupted` (retryable) —
  a crashed runner never leaves a task suspended forever
- Redfish first, IPMI fallback; `fake` driver for development and CI
- Two boot carriers per install: BMC virtual media (default) or PXE/iPXE
  network boot (built-in proxyDHCP + TFTP, Pixiecore-style; opt-in via
  `MAMMOTH_PXE_ENABLED` — docs/operations.md §4.5)

## Delivery

| Image | Base | Modes | Notes |
|-------|------|-------|-------|
| `mammoth` | distroless (static) | all / api / runner / prober | no external tools |
| `mammoth-builder` | alpine + xorriso | builder | privileged |

```bash
docker compose -f deploy/compose.all-in-one.yml up -d   # 1×all + PG
docker compose -f deploy/compose.faceted.yml up -d      # 1×api + N×runner + 1×builder
```

Restricted networks: `--build-arg RUNTIME_IMAGE=<mirror>/distroless/static-debian12:nonroot`
and `--build-arg GOPROXY=https://goproxy.cn,direct`.

## Official console

Prefer a UI over cURL? **[mammoth-console](https://github.com/3th1nk/mammoth-console)**
is the official web console for this engine — a single-page Vue 3 app with zero
private backend: every capability boundary is driven by `GET /api/v1`
(capabilities), and every action goes through the same public API you just used.

Machine lifecycle (register / auto-discovery / power / health & SEL / BIOS / drive
erase with two-stage confirmations), a four-step install wizard with an
install-plan dry run, live task & log streaming (SSE), zero-registration
onboarding, event audit with HMAC-signed webhooks, a ⌘K command palette, and
dark mode. Run it beside the engine with `docker compose`, or `npm run dev` for
local development.

![Mammoth Console](https://github.com/3th1nk/mammoth-console/raw/main/docs/screenshots/machines.png)

## Development

```bash
make build        # bin/mammoth
make test         # unit tests (no external services)
make test-pg      # queue/store contract suites against a disposable PG
make generate     # regenerate from api/openapi.yaml (contract drift fails CI)
make acceptance   # full scripted acceptance flow against a local all-in-one
```

Observability: JSON logs with standard fields (`task_id` `machine_id` `job_id`
`request_id` `stage`), Prometheus at `/metrics`, OTel boundary spans (no-op by
default, `MAMMOTH_OTEL_EXPORTER_ENDPOINT` to export).

## License

Apache-2.0

> Note: `assets/win-apply/` bundles GPL-2.0+ binaries (wimlib / mkntfs and
> supporting libraries) taken verbatim from the Alpine Linux repositories —
> sources and licenses are documented in
> [assets/win-apply/PROVENANCE.md](assets/win-apply/PROVENANCE.md).

### Logo

The mammoth gopher is a derivative of the original Go gopher by
**Renée French** (CC BY 3.0), adapted under the same license.
See `assets/brand/`.

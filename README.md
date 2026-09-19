<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/brand/logo-dark.svg">
    <img src="assets/brand/logo.svg" alt="mammoth logo" width="140">
  </picture>
</p>

# Mammoth

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
- Status: **v1.0 ready** — M0~M6 delivered (contract frozen); three distros
  (rocky9 / ubuntu22 / debian12) closed-loop on real hardware; M7 PXE/iPXE
  network boot delivered and closed-loop on real hardware. ([roadmap](docs/09-roadmap.md))

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
        ai["autoinstall<br/>ubuntu 22.04 / 24.04"]
        ps["preseed<br/>debian 12 / 13"]
        wu["unattend<br/>windows 2019 (UEFI-only)"]
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

### PXE addressing: the DHCP decision framework

The install L2 decides the mode — declared once per deployment, never
guessed per install (dual-DHCP races are unwinnable in code):

```mermaid
flowchart TD
    q{"Does a site DHCP server already<br/>serve the install segment?"}
    q -- "no" --> pool["**POOL mode**<br/>set MAMMOTH_PXE_DHCP_POOL —<br/>mammoth owns the wire:<br/>· answers DHCP for PXE ROMs<br/>  and installers<br/>· arm-time reservation: ping +<br/>  neighbour probe skips<br/>  occupied static addresses<br/>· leases only for MACs with<br/>  an armed install<br/>· boot and target use the<br/>  reserved address (ip= args)"]
    q -- "yes" --> proxy["**PROXY mode** — no pool<br/>site DHCP owns addresses:<br/>· site DHCP answers the<br/>  boot-phase IP<br/>· mammoth adds only PXE<br/>  boot options (67/4011)<br/>· declare the install address<br/>  in the spec: static ip= args<br/>  + target netplan<br/>· verify targets that address"]
    pool --> vlan["**cross-VLAN**<br/>DHCP relay (ip helper) on the<br/>machine segment forwards to<br/>mammoth; replies follow giaddr<br/>(RFC 2131). TFTP/HTTP are unicast —<br/>NextServer and media/API URLs<br/>must be routable from the<br/>machine VLAN"]
    proxy --> vlan
```

### Distro adaptation matrix — which image, which carrier

| Distro | Dialect | virtual_media image | PXE image | PXE install source | Real hardware |
|---|---|---|---|---|---|
| rocky 9 | kickstart | minimal / DVD ISO (repacked) | same ISO (boot files extracted) | HTTP pool or NFS ISO | ✅ both |
| rocky 10 | kickstart | DVD ISO, UEFI-only layout | same | same | qemu ✅ · real pending |
| centos 7 | kickstart | minimal ISO | same ISO | same | ✅ vMedia |
| kylin V10 / V11 | kickstart | DVD ISO | same ISO | same | pending |
| UOS | kickstart | DVD ISO | same ISO | NFS ISO | ✅ both |
| ubuntu 22.04 / 24.04 | autoinstall | **live-server** ISO (casper, repacked) | **live-server** ISO (squashfs over NFS) | unpacked ISO tree over NFS | ✅ both |
| debian 12 / 13 | preseed | **netinst** ISO (repacked) | **netinst** ISO (signed HTTP pool) **+ official netboot.tar.gz** + staged udebs | HTTP pool (checksum-complete, by-hash backfilled) | ✅ both |
| windows 2019 | unattend | official media repacked (root autounattend + SetupComplete wimlib injection, UDF bridge) | — (not in v1; WinPE chain deferred to v1.x) | — | qemu boot+unattend-accept ✅ · real pending |

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
| Ubuntu-like | Ubuntu 22.04.5 & 24.04.x live-server ISO | virtual_media ✅ · PXE ✅ |
| Debian-like | Debian 12 / 13 netinst ISO | virtual_media ✅ · PXE ✅ |
| Windows-like | Windows Server 2019 (zh-CN MSDN) | virtual_media 🔧 (UEFI-only, Standard Core) · qemu boot+unattend-accept ✅ · real pending |
| Extension | Rocky 10 (UEFI-only) · CentOS 7 (legacy) · Kylin V10/V11 · UOS | per-demand |

Every regression run: six-stage pipeline green → unattended first boot →
SSH probe with the provisioned key. See
[docs/runbooks/test-baselines.md](docs/runbooks/test-baselines.md).

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
and invalid values fail startup instead of silently defaulting:

```bash
bin/mammoth serve --env-file /etc/mammoth.env   # or MAMMOTH_ENV_FILE=...
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
# 3. power it on (async: 202 + job)
curl -s -X POST -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"power_on"}' localhost:8080/api/v1/machines/mch_.../actions
# 4. watch the job
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

### Logo

The mammoth gopher is a derivative of the original Go gopher by
**Renée French** (CC BY 3.0), adapted under the same license.
See `assets/brand/`.

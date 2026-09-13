<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/brand/logo-dark.svg">
  <img src="assets/brand/logo.svg" alt="mammoth logo" width="140" align="right">
</picture>

# Mammoth

> A self-contained bare-metal provisioning engine.
> Send in an address and a credential, get back a machine that runs.

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

#!/usr/bin/env python3
"""M0 acceptance flow (docs/09-roadmap.md).

Drives a running mammoth instance (mode=all) through the full acceptance
path with the fake BMC driver:

  1. register credential + a batch of machines
  2. submit a batch power_on job            → 202 + job
  3. wait for completion                    → job succeeded, machines power on
  4. exercise set_boot_device / mount_media / console / discover
  5. crash path: submit a slow job, SIGKILL the server, restart, verify the
     reaper marks the orphaned tasks interrupted and retry succeeds

Usage:
  python3 scripts/acceptance.py --api http://localhost:8080 --token devtoken
                                --dsn postgres://... (for the crash test)
  [ --skip-crash ]  [ --machines N ]
"""
import argparse
import json
import os
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.request

PASS = "✓"
FAIL = "✗"


class Client:
    def __init__(self, base, token):
        self.base = base.rstrip("/")
        self.token = token

    def req(self, method, path, body=None, headers=None):
        data = json.dumps(body).encode() if body is not None else None
        r = urllib.request.Request(self.base + path, data=data, method=method)
        r.add_header("Authorization", "Bearer " + self.token)
        if body is not None:
            r.add_header("Content-Type", "application/json")
        for k, v in (headers or {}).items():
            r.add_header(k, v)
        try:
            with urllib.request.urlopen(r, timeout=120) as resp:
                payload = resp.read()
                return resp.status, json.loads(payload) if payload else {}
        except urllib.error.HTTPError as e:
            payload = e.read()
            try:
                return e.code, json.loads(payload)
            except Exception:
                return e.code, {"raw": payload.decode(errors="replace")}

    def get(self, path):
        return self.req("GET", path)

    def post(self, path, body=None, headers=None):
        return self.req("POST", path, body, headers)


def check(name, ok, detail=""):
    mark = PASS if ok else FAIL
    print(f"  {mark} {name}" + (f" — {detail}" if detail else ""))
    return ok


def wait_job(c, job_id, want_states, timeout=120):
    """Poll a job until its state is in want_states; returns (job, tasks).

    Tolerates connection refusals: the crash stage polls across a restart.
    """
    deadline = time.time() + timeout
    job, tasks = {}, []
    while time.time() < deadline:
        try:
            _, job = c.get(f"/api/v1/jobs/{job_id}")
            if job.get("state") in want_states:
                _, tl = c.get(f"/api/v1/jobs/{job_id}/tasks?page_size=200")
                tasks = tl.get("items", [])
                return job, tasks
        except (urllib.error.URLError, ConnectionError, OSError):
            pass  # server down (crash stage) — keep polling
        time.sleep(0.5)
    return job, tasks


def wait_tasks(c, job_id, want_state, timeout=120):
    """Poll a job's tasks until any reaches want_state (crash-stage helper)."""
    deadline = time.time() + timeout
    tasks = []
    while time.time() < deadline:
        try:
            _, tl = c.get(f"/api/v1/jobs/{job_id}/tasks?page_size=200")
            tasks = tl.get("items", [])
            if any(t["state"] == want_state for t in tasks):
                return tasks
        except (urllib.error.URLError, ConnectionError, OSError):
            pass
        time.sleep(0.5)
    return tasks


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--api", default="http://localhost:8080")
    ap.add_argument("--token", default=os.environ.get("MAMMOTH_API_TOKEN", "devtoken"))
    ap.add_argument("--machines", type=int, default=3)
    ap.add_argument("--skip-crash", action="store_true",
                    help="skip the SIGKILL/reaper stage (needs --dsn)")
    ap.add_argument("--dsn", default="", help="database URL for crash-stage restart")
    ap.add_argument("--binary", default="/tmp/mammoth")
    args = ap.parse_args()

    c = Client(args.api, args.token)
    ok = True

    # 0. health
    print("· health")
    r = c.req("GET", "/healthz")
    ok &= check("GET /healthz → 204", r[0] == 204, str(r[0]))
    r = c.req("GET", "/readyz")
    ok &= check("GET /readyz → 204", r[0] == 204, str(r[0]))
    import urllib.request as u
    req = u.Request(c.base + "/api/v1/machines")
    try:
        with u.urlopen(req, timeout=10) as resp:
            status = resp.status
    except urllib.error.HTTPError as e:
        status = e.code
    ok &= check("unauthenticated /machines → 401", status == 401, str(status))

    # 1. register
    print("· registration")
    status, cred = c.post("/api/v1/credentials", {
        "type": "bmc", "name": f"acceptance-{time.time()}",
        "secret": {"username": "admin", "password": "s3cret"}})
    ok &= check("credential created", status == 201, f"{status}")
    ok &= check("secret not echoed", "secret" not in json.dumps(cred))
    cred_id = cred["id"]

    machines = []
    for i in range(args.machines):
        status, m = c.post("/api/v1/machines", {
            "labels": {"env": "acceptance", "rack": f"R{i}"},
            "bmc": {"address": f"fake://acc-node{i}", "protocol": "fake",
                    "credential_id": cred_id}})
        ok &= check(f"machine {i} registered", status == 201, m.get("id", str(status)))
        machines.append(m["id"])

    # 2. batch power_on
    print("· batch power_on")
    status, job = c.post("/api/v1/jobs", {
        "type": "power", "targets": {"machine_ids": machines},
        "action": {"type": "power_on"}})
    ok &= check("batch job accepted (202)", status == 202, f"{status}")
    job_id = job["id"]
    job, tasks = wait_job(c, job_id, {"succeeded"}, timeout=90)
    ok &= check("job succeeded", job.get("state") == "succeeded", job.get("state", ""))
    ok &= check("3 tasks terminal",
                len(tasks) == args.machines and all(t["state"] == "succeeded" for t in tasks))
    ok &= check("summary materialized",
                job.get("summary", {}).get("succeeded") == args.machines,
                json.dumps(job.get("summary", {})))

    pstates = [c.get(f"/api/v1/machines/{m}")[1]["power_state"] for m in machines]
    ok &= check("machines power_state=on", all(p == "on" for p in pstates), str(pstates))

    # 3. boot device + media + console + discover
    print("· actions")
    status, bjob = c.post(f"/api/v1/machines/{machines[0]}/actions",
                          {"type": "set_boot_device", "device": "pxe", "once": True})
    ok &= check("set_boot_device → 202 job", status == 202, f"{status}")
    _, _ = wait_job(c, bjob["id"], {"succeeded", "failed"}, timeout=60)

    status, mjob = c.post(f"/api/v1/machines/{machines[0]}/actions",
                          {"type": "mount_media", "image_url": "https://mirror.example/rocky9.iso"})
    ok &= check("mount_media → 202 job", status == 202, f"{status}")
    _, _ = wait_job(c, mjob["id"], {"succeeded", "failed"}, timeout=60)

    status, console = c.get(f"/api/v1/machines/{machines[0]}/console")
    ok &= check("console URL", status == 200 and console.get("url", "").startswith("https://"),
                str(console.get("url", status)))

    status, djob = c.post(f"/api/v1/machines/{machines[0]}/actions", {"type": "discover"})
    _, _ = wait_job(c, djob["id"], {"succeeded", "failed"}, timeout=60)
    _, machine = c.get(f"/api/v1/machines/{machines[0]}")
    ok &= check("discover backfilled vendor/model, state=ready",
                machine["state"] == "ready" and machine["bmc"].get("vendor") == "acme",
                f"state={machine['state']} vendor={machine['bmc'].get('vendor')}")

    # 4. idempotency
    print("· idempotency")
    key = f"acc-{time.time()}"
    _, j1 = c.post("/api/v1/jobs", {"type": "power", "targets": {"machine_ids": [machines[1]]},
                                    "action": {"type": "power_off"}}, headers={"Idempotency-Key": key})
    _, j2 = c.post("/api/v1/jobs", {"type": "power", "targets": {"machine_ids": [machines[1]]},
                                    "action": {"type": "power_off"}}, headers={"Idempotency-Key": key})
    ok &= check("replay returns the original job", j1["id"] == j2["id"], f"{j1['id']} vs {j2['id']}")

    # 5. crash path: requires the outer environment to run this instance with
    # MAMMOTH_FAKE_BMC_DELAY set (tasks slow enough to kill mid-flight) and
    # --dsn pointing at the same database for the restart.
    if not args.skip_crash:
        print("· crash → interrupted → retry")
        if not args.dsn:
            print("  ✗ skipped: --dsn required")
            ok = False
        else:
            env = dict(os.environ)
            env["MAMMOTH_DATABASE_URL"] = args.dsn
            env["MAMMOTH_API_TOKEN"] = args.token
            if not env.get("MAMMOTH_FAKE_BMC_DELAY"):
                print("  ✗ skipped: set MAMMOTH_FAKE_BMC_DELAY (e.g. 8s) so tasks are slow")
                ok = False
            else:
                status, cjob = c.post("/api/v1/jobs", {
                    "type": "power", "targets": {"machine_ids": [machines[2]]},
                    "action": {"type": "cycle"}})
                job_id2 = cjob["id"]
                # wait until the task is running, then SIGKILL the server mid-flight
                state = ""
                deadline = time.time() + 30
                while time.time() < deadline:
                    _, tl = c.get(f"/api/v1/jobs/{job_id2}/tasks")
                    items = tl.get("items", [])
                    state = items[0]["state"] if items else ""
                    if state == "running":
                        break
                    time.sleep(0.2)
                subprocess.run(["pkill", "-9", "-f", "mammoth serve"], check=False)
                time.sleep(1)
                ok &= check("task was running at SIGKILL", state == "running", state)

                # restart on the same database; the reaper must mark the
                # orphaned task interrupted
                subprocess.Popen([args.binary, "serve", "--mode=all"],
                                 stdout=subprocess.DEVNULL, stderr=subprocess.STDOUT, env=env)
                # The job itself stays "running" (interrupted tasks are
                # retryable, not terminal); the task state is the signal.
                tasks2 = wait_tasks(c, job_id2, "interrupted", timeout=60)
                ok &= check("reaper marked task interrupted",
                            any(t["state"] == "interrupted" for t in tasks2),
                            str([t["state"] for t in tasks2]))

                # retry: re-enters at the persisted stage index and succeeds
                if any(t["state"] == "interrupted" for t in tasks2):
                    tid = next(t["id"] for t in tasks2 if t["state"] == "interrupted")
                    status, _ = c.post(f"/api/v1/jobs/{job_id2}/tasks/{tid}/retry")
                    ok &= check("retry accepted", status == 202, str(status))
                    job2, tasks2 = wait_job(c, job_id2, {"succeeded", "failed"}, timeout=120)
                    ok &= check("retried task succeeded",
                                tasks2 and tasks2[0]["state"] == "succeeded",
                                tasks2[0]["state"] if tasks2 else "?")

    print()
    print("ACCEPTANCE:", "PASS" if ok else "FAIL")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()

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
                if not payload:
                    return resp.status, {}
                try:
                    return resp.status, json.loads(payload)
                except Exception:
                    return resp.status, payload.decode(errors="replace")
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


def wait_machine(c, machine_id, want, timeout=60):
    """Poll a machine until its state reaches `want` (or error)."""
    deadline = time.time() + timeout
    m = {}
    while time.time() < deadline:
        _, m = c.get(f"/api/v1/machines/{machine_id}")
        if m.get("state") == want:
            return m
        time.sleep(0.5)
    return m


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

    # M1 acceptance: complete spec view within 30s of registration
    # (docs/09-roadmap.md M1) — auto-discovery must fill hardware.
    print("· spec view readiness (30s window)")
    started = time.time()
    for m in machines:
        ready = wait_machine(c, m, "ready", timeout=30)
        elapsed = time.time() - started
        hw = ready.get("hardware") or {}
        ok &= check(f"{m} ready in {elapsed:.0f}s",
                    ready.get("state") == "ready" and elapsed <= 30,
                    f"state={ready.get('state')}")
        ok &= check(f"{m} hardware view complete",
                    len(hw.get("disks", [])) == 3 and hw.get("coverage") == "full"
                    and (hw.get("cpu") or {}).get("cores", 0) > 0,
                    f"disks={len(hw.get('disks', []))} coverage={hw.get('coverage')} "
                    f"cores={(hw.get('cpu') or {}).get('cores')}")

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

    # 3.4 partition-level discovery (M2): a machine WITH ssh access configured
    # but unreachable in-band must yield an explicit classified error — never
    # a hanging task — while keeping its ready spec view. A machine without
    # ssh config simply has no snapshot.
    print("· in-band layout (explicit failure, no hang)")
    status, sshcred = c.post("/api/v1/credentials", {
        "type": "ssh", "name": f"acc-ssh-{time.time()}",
        "secret": {"username": "root", "password": "whatever"}})
    ok &= check("ssh credential created", status == 201, str(status))
    status, sshm = c.post("/api/v1/machines", {
        "labels": {"env": "acceptance"},
        "bmc": {"address": "fake://acc-ssh-node", "protocol": "fake",
                "credential_id": cred_id},
        "ssh_credential_id": sshcred["id"],
        "ssh": {"address": "127.0.0.1:1"}})   # connection refused instantly
    if status == 201:
        mid = sshm["id"]
        t0 = time.time()
        done = wait_machine(c, mid, "ready", timeout=60)
        elapsed = time.time() - t0
        code = (done.get("last_error") or {}).get("code", "")
        ok &= check("in-band failure surfaces as NETWORK_UNREACHABLE",
                    done.get("state") == "ready" and code == "NETWORK_UNREACHABLE",
                    f"state={done.get('state')} code={code} in {elapsed:.0f}s")
        ok &= check("no snapshot when in-band failed", True, "layout endpoint below")
        status, _ = c.get(f"/api/v1/machines/{mid}/layout")
        ok &= check("GET layout → 404 without snapshot", status == 404, str(status))
    else:
        ok &= check("in-band test machine registered", False, str(status))

    status, body = c.get(f"/api/v1/machines/{machines[0]}/layout")
    ok &= check("machine without ssh config: layout 404", status == 404, str(status))

    # 3.5 classified BMC error on unreachable target (M1 acceptance:
    # "BMC credential errors get classified error codes" — same classified
    # path carries unreachable).
    print("· error classification")
    status, bad = c.post("/api/v1/machines", {
        "labels": {"env": "acceptance"},
        "bmc": {"address": "192.0.2.1", "protocol": "redfish",
                "credential_id": cred_id}})
    if status == 201:
        errored = wait_machine(c, bad["id"], "error", timeout=60)
        code = (errored.get("last_error") or {}).get("code", "")
        ok &= check("unreachable BMC → state=error with BMC_UNREACHABLE",
                    errored.get("state") == "error" and code == "BMC_UNREACHABLE",
                    f"state={errored.get('state')} code={code}")
    else:
        ok &= check("error-classification machine registered", False, str(status))

    # 4. idempotency
    print("· idempotency")
    key = f"acc-{time.time()}"
    _, j1 = c.post("/api/v1/jobs", {"type": "power", "targets": {"machine_ids": [machines[1]]},
                                    "action": {"type": "power_off"}}, headers={"Idempotency-Key": key})
    _, j2 = c.post("/api/v1/jobs", {"type": "power", "targets": {"machine_ids": [machines[1]]},
                                    "action": {"type": "power_off"}}, headers={"Idempotency-Key": key})
    ok &= check("replay returns the original job", j1["id"] == j2["id"], f"{j1['id']} vs {j2['id']}")

    # 4.5 install flow (M3): batch Rocky install on fake machines. The
    # acceptance script plays the machine: fetch the rendered kickstart via
    # the task-token URL, then report completion like the %post hook does.
    print("· install flow (five stages)")
    status, ijob = c.post("/api/v1/jobs", {
        "type": "install",
        "targets": {"machine_ids": [machines[0], machines[1]]},
        "spec": {
            "image": {"source": "https://mirror.example/rocky9.iso", "distro": "rocky9"},
            "storage": {"disks": [{
                "select": {"match": {"type": "nvme", "size": "largest"}},
                "wipe": True,
                "partitions": [
                    {"size": "512M", "fs": "vfat", "mount": "/boot/efi", "flags": ["esp"]},
                    {"size": "rest", "fs": "xfs", "mount": "/"}]}]},
            "network": [{
                "bond": {"interfaces": [
                    {"match": {"mac": "aa:bb:cc:dd:ee:01"}},
                    {"match": {"mac": "aa:bb:cc:dd:ee:02"}}],
                    "mode": "802.3ad", "params": {"miimon": 100}},
                "addresses": ["172.16.1.11/24"],
                "routes": [{"to": "default", "via": "172.16.1.1"}],
                "nameservers": {"addresses": ["10.0.0.53"]}}],
            "identity": {"hostname_pattern": "node-{index}"},
            "access": {"root_password": "generate",
                       "ssh_keys": ["ssh-ed25519 AAA acceptance@mammoth"]},
            "network": [{
                "bond": {"interfaces": [
                    {"match": {"mac": "aa:bb:cc:dd:ee:01"}},
                    {"match": {"mac": "aa:bb:cc:dd:ee:02"}}],
                    "mode": "802.3ad", "params": {"miimon": 100}},
                "addresses": ["172.16.1.11/24"],
                "routes": [{"to": "default", "via": "172.16.1.1"}]}],
            "scripts": [{"stage": "post_install",
                         "content_base64": "ZWNobyBtYW1tb3RoLXNldHVwLWRvbmUK"}]},
        "policy": {"concurrency": 2, "on_task_failure": "continue"}})
    ok &= check("install job accepted", status == 202, f"{status}")
    ijob_id = ijob["id"]

    def play_machine(task):
        """Act as the target machine: fetch ks, report completion."""
        url = task.get("answer_url")
        if not url:
            return False
        base = url.rsplit("/render/", 1)
        if len(base) != 2:
            return False
        token = base[1].split("/", 1)[0]
        status, body = c.req("GET", f"/render/{token}/ks.cfg")
        if status != 200 or "rootpw --plaintext" not in body:
            return False
        status, _ = c.req("POST", f"/render/{token}/complete",
                          {"status": "ok", "detail": "acceptance machine"})
        return status == 204

    deadline = time.time() + 180
    played = set()
    final = {}
    while time.time() < deadline:
        _, tl = c.get(f"/api/v1/jobs/{ijob_id}/tasks?page_size=200")
        items = tl.get("items", [])
        for t in items:
            if t["id"] not in played and t.get("answer_url"):
                # fetch once when answers exist (machine can fetch any time
                # after prepare_media); report completion immediately — the
                # install stage polls the record.
                if play_machine(t):
                    played.add(t["id"])
        if items and all(t["state"] in ("succeeded", "failed", "canceled") for t in items):
            final = {"tasks": items}
            break
        time.sleep(1)
    _, ijob_final = c.get(f"/api/v1/jobs/{ijob_id}")
    ok &= check("both install tasks succeeded",
                all(t["state"] == "succeeded" for t in final.get("tasks", [])),
                str([t["state"] for t in final.get("tasks", [])]))
    ok &= check("five stages all green",
                all(s["state"] == "succeeded" for t in final.get("tasks", [])
                    for s in t.get("stages", [])) and
                len(final.get("tasks", [{}])[0].get("stages", [])) == 5,
                str([s["name"] + ":" + s["state"] for s in
                     (final.get("tasks") or [{}])[0].get("stages", [])]))

    # kickstart content assertions via one played machine's answer URL
    if played:
        t0 = next(t for t in final.get("tasks", []) if t.get("answer_url"))
        token = t0["answer_url"].rsplit("/render/", 1)[1].split("/", 1)[0]
        _, ks = c.req("GET", f"/render/{token}/ks.cfg")
        ok &= check("kickstart: random root password (not literal 'generate')",
                    "rootpw --plaintext " in ks and "generate" not in ks)
        ok &= check("kickstart: bond resolved by MAC in pre hook",
                    "iface_by_mac aa:bb:cc:dd:ee:01" in ks and "--bondslaves=$bond_slaves" in ks)
        import re as _re
        ok &= check("kickstart: ssh key + hostname expanded",
                    "ssh-ed25519 AAA acceptance@mammoth" in ks
                    and "{index}" not in ks
                    and _re.search(r"hostnamectl set-hostname node-\d+", ks) is not None)
        ok &= check("kickstart: wipe path on resolved nvme",
                    "clearpart --drives=nvme0n1 --initlabel --all" in ks,
                    "clearpart" in ks)

    # abort_batch: one impossible target (per-machine override) fails
    # verify_layout → the sibling is canceled while waiting in install_os
    status, ajob = c.post("/api/v1/jobs", {
        "type": "install",
        "targets": {
            "machine_ids": [machines[1], machines[2]],
            "overrides": {
                # impossible selector for machine[1] only
                machines[1]: {"storage": {"disks": [{
                    "select": {"match": {"serial": "DOES-NOT-EXIST"}},
                    "wipe": True,
                    "partitions": [{"size": "rest", "fs": "xfs", "mount": "/"}]}]}},
            }},
        "spec": {
            "image": {"source": "https://mirror.example/rocky9.iso", "distro": "rocky9"},
            "storage": {"disks": [{
                "select": {"match": {"type": "nvme", "size": "largest"}},
                "wipe": True,
                "partitions": [{"size": "rest", "fs": "xfs", "mount": "/"}]}]}},
        "policy": {"on_task_failure": "abort_batch"}})
    ok &= check("abort_batch job accepted", status == 202, f"{status}")
    deadline = time.time() + 120
    astate = {}
    while time.time() < deadline:
        _, tl = c.get(f"/api/v1/jobs/{ajob['id']}/tasks?page_size=200")
        items = tl.get("items", [])
        if items and all(t["state"] in ("failed", "canceled", "succeeded") for t in items):
            astate = {"tasks": items}
            break
        time.sleep(1)
    _, ajob_final = c.get(f"/api/v1/jobs/{ajob['id']}")
    ok &= check("failed selector → task failed LAYOUT_DISK_NOT_FOUND",
                any(t["state"] == "failed" and (t.get("error") or {}).get("code") == "LAYOUT_DISK_NOT_FOUND"
                    for t in astate.get("tasks", [])),
                str([(t["state"], (t.get("error") or {}).get("code")) for t in astate.get("tasks", [])]))
    ok &= check("abort_batch: sibling task canceled",
                any(t["state"] == "canceled" for t in astate.get("tasks", [])) and
                ajob_final.get("state") in ("partial", "canceled", "failed"),
                f"job={ajob_final.get('state')}")

    # retry a failed install task (re-enters at verify_layout)
    failed_tid = next((t["id"] for t in astate.get("tasks", []) if t["state"] == "failed"), None)
    if failed_tid:
        status, _ = c.post(f"/api/v1/jobs/{ajob['id']}/tasks/{failed_tid}/retry")
        ok &= check("failed install task retry accepted", status == 202, str(status))

    # 4.7 M4: keep:partitions install — partition-level reuse with the %pre
    # drift guard. The fake in-band probe supplies the snapshot; the script
    # plays the machine (fetch ks + report completion / drift report).
    print("· keep:partitions install + drift guard")
    status, keepm = c.post("/api/v1/machines", {
        "labels": {"env": "acceptance"},
        "bmc": {"address": "fake://acc-keep-node", "protocol": "fake",
                "credential_id": cred_id},
        "ssh_credential_id": sshcred["id"],
        "ssh": {"address": "fake://inband"}})
    if status == 201:
        keepm_id = keepm["id"]
        wait_machine(c, keepm_id, "ready", timeout=60)
        # the snapshot lands ~1 beat after ready (in-band collection runs
        # after the spec view is persisted) — poll it
        status, layout = 0, {}
        deadline = time.time() + 30
        while time.time() < deadline:
            status, layout = c.get(f"/api/v1/machines/{keepm_id}/layout")
            if status == 200:
                break
            time.sleep(0.5)
        ok &= check("layout snapshot captured via fake in-band",
                    status == 200 and any(
                        d.get("match", {}).get("serial") == "GIM256_0001"
                        for d in layout.get("disks", [])),
                    str(status))
        status, kjob = c.post("/api/v1/jobs", {
            "type": "install",
            "targets": {"machine_ids": [keepm_id]},
            "spec": {
                "image": {"source": "https://mirror.example/rocky9.iso", "distro": "rocky9"},
                "storage": {"disks": [
                    {"select": {"match": {"serial": "S6XPN0001"}}, "wipe": True,
                     "partitions": [
                         {"size": "512M", "fs": "vfat", "mount": "/boot/efi", "flags": ["esp"]},
                         {"size": "rest", "fs": "xfs", "mount": "/"}]},
                    {"select": {"match": {"serial": "GIM256_0001"}}, "keep": "partitions",
                     "preserve": [{"number": 1, "mount": "/data"}],
                     "partitions": [{"size": "rest", "fs": "xfs", "mount": "/extra"}]}]}},
            "access": {"ssh_keys": ["ssh-ed25519 AAA keep@mammoth"]},
            "policy": {"on_task_failure": "continue"}})
        ok &= check("keep install accepted", status == 202, f"{status}")
        ktid, kurl = None, None
        deadline = time.time() + 120
        while time.time() < deadline and kurl is None:
            _, tl = c.get(f"/api/v1/jobs/{kjob['id']}/tasks")
            for t in tl.get("items", []):
                if t.get("answer_url"):
                    ktid, kurl = t["id"], t["answer_url"]
                    break
            time.sleep(0.5)
        ok &= check("answers rendered (task reached prepare_media)", kurl is not None)
        if kurl:
            token = kurl.rsplit("/render/", 1)[1].split("/", 1)[0]
            _, kks = c.req("GET", f"/render/{token}/ks.cfg")
            ok &= check("preserve: unformatted reuse by original partition",
                        "part /data --onpart=sda1 --noformat" in kks, "onpart line")
            ok &= check("wipe disk only on the system drive",
                        "clearpart --drives=nvme0n1 --initlabel --all" in kks
                        and "--drives=nvme0n1,sda" not in kks)
            ok &= check("drift guard bound to the snapshot baseline",
                        "_d=/sys/block/sda/sda1" in kks
                        and 'LAYOUT_DRIFT: ' in kks
                        and '"$(blkid -s UUID -o value /dev/sda1)" = "b2a1c3d4-0000-1111-2222-333344445555"' in kks)
            # complete ok like the %post hook
            c.req("POST", f"/render/{token}/complete", {"status": "ok"})
            kjob_f, ktasks = wait_job(c, kjob["id"], {"succeeded", "failed", "partial"}, timeout=180)
            ok &= check("keep install five stages green",
                        ktasks and ktasks[0]["state"] == "succeeded"
                        and all(st["state"] == "succeeded" for st in ktasks[0].get("stages", [])),
                        str([st["state"] for st in (ktasks[0].get("stages") if ktasks else [])]))

            # drift: the %pre guard reports LAYOUT_DRIFT when the live table
            # deviates from the baseline; simulate exactly that report path.
            status, djob = c.post("/api/v1/jobs", {
                "type": "install",
                "targets": {"machine_ids": [keepm_id]},
                "spec": {
                    "image": {"source": "https://mirror.example/rocky9.iso", "distro": "rocky9"},
                    "storage": {"disks": [
                        {"select": {"match": {"serial": "S6XPN0001"}}, "wipe": True,
                         "partitions": [{"size": "rest", "fs": "xfs", "mount": "/"}]},
                        {"select": {"match": {"serial": "GIM256_0001"}}, "keep": "partitions",
                         "preserve": [{"number": 1, "mount": "/data"}]}]}},
                "policy": {"on_task_failure": "continue"}})
            dtid, durl = None, None
            deadline = time.time() + 120
            while time.time() < deadline and durl is None:
                _, tl = c.get(f"/api/v1/jobs/{djob['id']}/tasks")
                for t in tl.get("items", []):
                    if t.get("answer_url"):
                        dtid, durl = t["id"], t["answer_url"]
                        break
                time.sleep(0.5)
            if durl:
                dtok = durl.rsplit("/render/", 1)[1].split("/", 1)[0]
                # the machine's %pre guard fires: report drift, exit 1
                c.req("POST", f"/render/{dtok}/complete",
                      {"status": "failed", "detail": "LAYOUT_DRIFT: sda1 start drifted"})
                djob_f, dtasks = wait_job(c, djob["id"], {"failed", "partial"}, timeout=120)
                err = (dtasks[0].get("error") or {}) if dtasks else {}
                ok &= check("drifted baseline → task fails with LAYOUT_DRIFT",
                            dtasks and dtasks[0]["state"] == "failed"
                            and "LAYOUT_DRIFT" in err.get("message", ""),
                            f"{err.get('code')}: {err.get('message', '')[:60]}")
                status, _ = c.post(f"/api/v1/jobs/{djob['id']}/tasks/{dtid}/retry")
                ok &= check("drift-failed task retry accepted (re-binds current snapshot)",
                            status == 202, str(status))
            else:
                ok &= check("drift job rendered", False)
    else:
        ok &= check("keep test machine registered", False, str(status))

    # 4.9 M5: second distro (ubuntu22 autoinstall) + support matrix gating.
    print("· ubuntu autoinstall + matrix")
    status, caps = c.get("/api/v1")
    distros = {d["name"]: d["keep_partition_support"] for d in caps.get("distros", [])}
    ok &= check("capabilities expose distro matrix",
                distros.get("rocky9") == "full" and distros.get("ubuntu22") == "partial",
                str(distros))

    # partial distro + keep:partitions → rejected at submit (docs/06 §5)
    status, rej = c.post("/api/v1/jobs", {
        "type": "install",
        "targets": {"machine_ids": [machines[0]]},
        "spec": {
            "image": {"source": "https://mirror.example/ubuntu-22.04.iso", "distro": "ubuntu22"},
            "storage": {"disks": [{
                "select": {"match": {"serial": "S6XPN0001"}}, "keep": "partitions",
                "preserve": [{"number": 1, "mount": "/data"}]}]}},
        "policy": {"on_task_failure": "continue"}})
    ok &= check("partial distro rejects keep:partitions at submit",
                status == 422 and (rej.get("code") == "SCHEMA_UNSUPPORTED_KEEP"),
                f"{status} {rej.get('code')}")

    # keep: disk IS allowed on a partial distro
    # keep: disk IS allowed on a partial distro
    status, ud_ok = c.post("/api/v1/jobs", {
        "type": "install",
        "targets": {"machine_ids": [machines[0]]},
        "spec": {
            "image": {"source": "https://mirror.example/ubuntu-22.04.iso", "distro": "ubuntu22"},
            "storage": {"disks": [
                {"select": {"match": {"serial": "S6XPN0001"}}, "wipe": True,
                 "partitions": [{"size": "rest", "fs": "xfs", "mount": "/"}]},
                {"select": {"match": {"serial": "GIM256_0001"}}, "keep": "disk"}]},
            "network": [{
                "bond": {"interfaces": [
                    {"match": {"mac": "aa:bb:cc:dd:ee:01"}},
                    {"match": {"mac": "aa:bb:cc:dd:ee:02"}}],
                    "mode": "802.3ad", "params": {"miimon": 100}},
                "addresses": ["172.16.2.11/24"]}],
        },
        "policy": {"on_task_failure": "continue"}})
    if status == 202:
        uurl = None
        deadline = time.time() + 120
        while time.time() < deadline and uurl is None:
            _, tl = c.get(f"/api/v1/jobs/{ud_ok['id']}/tasks")
            for t in tl.get("items", []):
                if t.get("answer_url"):
                    uurl = t["answer_url"]
                    break
            time.sleep(0.5)
        if uurl:
            utok = uurl.rsplit("/render/", 1)[1].split("/", 1)[0]
            uurl_base = uurl.rsplit("/render/", 1)[0] + f"/render/{utok}"
            _, meta = c.req("GET", f"/render/{utok}/meta-data")
            _, udata = c.req("GET", f"/render/{utok}/user-data")
            ok &= check("nocloud seed: meta-data + user-data served",
                        meta is not None and "#cloud-config" in udata)
            ok &= check("autoinstall: netplan bond matched by MAC",
                        '"macaddress": "aa:bb:cc:dd:ee:01"' in udata
                        and '"mode": "802.3ad"' in udata)
            ok &= check("autoinstall: curtin storage keeps the kept disk out",
                        '"/dev/sdb"' not in udata and '"/dev/nvme0n1"' in udata)
            ok &= check("autoinstall: completion callback in late-commands",
                        f"/render/{utok}/complete" in udata)
            c.req("POST", f"/render/{utok}/complete", {"status": "ok"})
            ujob_f, utasks = wait_job(c, ud_ok["id"], {"succeeded", "failed", "partial"}, timeout=180)
            ok &= check("ubuntu install five stages green",
                        utasks and utasks[0]["state"] == "succeeded"
                        and all(st["state"] == "succeeded" for st in utasks[0].get("stages", [])),
                        str([st["state"] for st in (utasks[0].get("stages") if utasks else [])]))

    # ramdisk probe: optional feature, explicit gating (docs/05 §4)
    status, rjob = c.post(f"/api/v1/machines/{machines[0]}/actions",
                          {"type": "discover", "probe": "ramdisk"})
    ok &= check("ramdisk discover accepted as job", status == 202, f"{status}")
    rjob_f, rtasks = wait_job(c, rjob["id"], {"failed", "succeeded", "partial"}, timeout=60)
    rerr = (rtasks[0].get("error") or {}) if rtasks else {}
    ok &= check("ramdisk probe explicitly unsupported",
                rtasks and rtasks[0]["state"] == "failed" and rerr.get("code") == "BMC_UNSUPPORTED",
                f"{rerr.get('code')}: {rerr.get('message', '')[:50]}")

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
                subprocess.run(["pkill", "-9", "-f", "serve --mode=all"], check=False)
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

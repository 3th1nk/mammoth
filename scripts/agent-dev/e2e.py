#!/usr/bin/env python3
"""Agent initramfs pilot end-to-end (docs/12-agent-initramfs.md §verification).

Drives a running mammoth (fake BMC machines) through the agent install path,
then plays the hardware in qemu — the agent runtime inside the guest does the
real work (partition, install from the pool, bootloader, callback):

  1. register credential + one fake machine
  2. submit an alpine install (boot.strategy=virtual_media, extended ISO)
  3. wait for prepare_media → boot-<token>.iso
  4. boot the ISO in qemu (bios or uefi) — the agent applies the plan
  5. agent POSTs the completion callback → pipeline continues unattended
  6. job green = six stages succeeded
  7. boot the same disk from grub → ssh in with the spec'd key → hostname

Usage (see run.sh):
  e2e.py --api http://localhost:8080 --token devtoken --media-dir <dir>
         --iso <alpine-extended.iso> --workdir /tmp/agent-e2e
         --firmware bios|uefi
"""
import argparse
import json
import os
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

    def req(self, method, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        r = urllib.request.Request(self.base + path, data=data, method=method)
        r.add_header("Authorization", "Bearer " + self.token)
        if body is not None:
            r.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(r, timeout=60) as resp:
                payload = resp.read()
                return resp.status, (json.loads(payload) if payload else {})
        except urllib.error.HTTPError as e:
            payload = e.read()
            try:
                return e.code, json.loads(payload)
            except Exception:
                return e.code, {"raw": payload.decode(errors="replace")}


def check(name, ok, detail=""):
    print(f"  {PASS if ok else FAIL} {name}" + (f" — {detail}" if detail else ""))
    return ok


def wait_for(fn, timeout, what, interval=2):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        last = fn()
        if last:
            return last
        time.sleep(interval)
    raise TimeoutError(f"timeout waiting for {what}: {last}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--api", default="http://localhost:8080")
    ap.add_argument("--token", default="devtoken")
    ap.add_argument("--iso", required=True, help="alpine extended ISO (local path)")
    ap.add_argument("--media-dir", required=True)
    ap.add_argument("--workdir", required=True)
    ap.add_argument("--firmware", choices=["bios", "uefi"], default="bios")
    ap.add_argument("--disk-gb", type=int, default=8)
    ap.add_argument("--timeout", type=int, default=900)
    args = ap.parse_args()

    c = Client(args.api, args.token)
    ok = True
    os.makedirs(args.workdir, exist_ok=True)

    status, _ = c.req("GET", "/healthz")
    ok &= check("mammoth healthy", status == 204, str(status))

    print("· register machine")
    status, cred = c.req("POST", "/api/v1/credentials", {
        "type": "bmc", "name": f"agent-e2e-{args.firmware}-{int(time.time())}",
        "secret": {"username": "admin", "password": "s3cret"}})
    ok &= check("credential created", status == 201, str(status))
    status, m = c.req("POST", "/api/v1/machines", {
        "bmc": {"address": f"fake://agent-node-{args.firmware}-{int(time.time())}", "protocol": "fake",
                "credential_id": cred["id"]}})
    ok &= check("machine registered", status == 201, m.get("id", str(status)))
    mid = m["id"]
    wait_for(lambda: c.req("GET", f"/api/v1/machines/{mid}")[1].get("state") == "ready",
             60, "machine ready")

    # spec key: the installed system must accept it (phase-7 verification)
    key_path = os.path.join(args.workdir, "id_ed25519")
    pub_path = key_path + ".pub"
    if not os.path.exists(key_path):
        subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key_path],
                       check=True)
    with open(pub_path) as f:
        ssh_key = f.read().strip()

    print("· submit alpine install (agent path)")
    status, job = c.req("POST", "/api/v1/jobs", {
        "type": "install",
        "targets": {"machine_ids": [mid]},
        "spec": {
            "boot": {"strategy": "virtual_media"},
            "image": {"source": "file://" + args.iso, "distro": "alpine"},
            "storage": {"disks": [{
                "select": {"match": {"type": "nvme", "size": "largest"}},
                "wipe": True,
                "partitions": [
                    {"size": "300M", "fs": "vfat", "mount": "/boot/efi", "flags": ["esp"]},
                    {"size": "512M", "fs": "swap"},
                    {"size": "rest", "fs": "ext4", "mount": "/"}]}]},
            "identity": {"hostname_pattern": f"agent-{args.firmware}-{{index}}"},
            "access": {"ssh_keys": [ssh_key]},
        }})
    ok &= check("install job accepted", status == 202, f"{status} {job.get('detail', '')}")
    job_id = job["id"]

    def tasks():
        return c.req("GET", f"/api/v1/jobs/{job_id}/tasks?page_size=10")[1].get("items", [])

    # prepare_media renders the plan and repacks the extended ISO
    t0 = time.time()
    token = wait_for(lambda: next(
        (t["answer_url"].rsplit("/render/", 1)[1].split("/", 1)[0]
         for t in tasks() if t.get("answer_url")), None),
        600, "prepare_media (plan + ISO)")
    print(f"  {PASS} prepare_media done in {time.time()-t0:.0f}s (token {token})")

    iso_file = os.path.join(args.media_dir, f"boot-{token}.iso")
    if not os.path.exists(iso_file):
        check("boot ISO exists", False, iso_file)
        return False
    plan = subprocess.run(
        ["xorriso", "-osirrox", "on", "-indev", iso_file, "-extract",
         "/agent-plan.sh", os.path.join(args.workdir, "agent-plan.sh")],
        capture_output=True)
    ok &= check("agent plan baked into ISO", plan.returncode == 0)

    disk = os.path.join(args.workdir, f"disk-{args.firmware}.raw")
    if os.path.exists(disk):
        os.remove(disk)
    subprocess.run(["qemu-img", "create", "-f", "raw", disk, f"{args.disk_gb}G"],
                   check=True, capture_output=True)
    serial_log = os.path.join(args.workdir, f"serial-{args.firmware}.log")

    cmd = ["qemu-system-x86_64", "-m", "2048", "-smp", "2",
           "-display", "none",
           "-serial", f"file:{serial_log}",
           "-drive", f"file={disk},format=raw,if=none,id=disk0",
           "-device", "nvme,drive=disk0,serial=agentdisk",
           "-netdev", "user,id=n0", "-device", "virtio-net-pci,netdev=n0",
           "-cdrom", iso_file, "-boot", "d", "-no-reboot"]
    if args.firmware == "uefi":
        fw = "/opt/homebrew/share/qemu"
        vars_copy = os.path.join(args.workdir, "vars.fd")
        vars_src = os.path.join(fw, "edk2-x86_64-vars.fd")
        if not os.path.exists(vars_src):
            # homebrew's qemu packaging keeps the x86_64 UEFI var store under
            # the i386 name (the var store is the 32-bit payload)
            vars_src = os.path.join(fw, "edk2-i386-vars.fd")
        subprocess.run(["cp", vars_src, vars_copy], check=True)
        cmd = ["qemu-system-x86_64", "-m", "2048", "-smp", "2",
               "-display", "none",
               "-serial", f"file:{serial_log}",
               "-drive", f"if=pflash,format=raw,readonly=on,file={fw}/edk2-x86_64-code.fd",
               "-drive", f"if=pflash,format=raw,file={vars_copy}",
               "-drive", f"file={disk},format=raw,if=none,id=disk0",
               "-device", "nvme,drive=disk0,serial=agentdisk",
               "-netdev", "user,id=n0", "-device", "virtio-net-pci,netdev=n0",
               "-cdrom", iso_file, "-boot", "d", "-no-reboot"]
    print(f"· boot agent ISO in qemu ({args.firmware})")
    qemu_log = open(serial_log, "w")
    qemu = subprocess.Popen(cmd, stdout=qemu_log, stderr=subprocess.STDOUT)

    def job_green():
        _, tasks_list = c.req("GET", f"/api/v1/jobs/{job_id}/tasks?page_size=10")
        items = tasks_list.get("items", [])
        if items and all(t["state"] in ("succeeded", "failed", "canceled") for t in items):
            return items
        return None

    try:
        items = wait_for(job_green, args.timeout, "install job terminal state")
    finally:
        try:
            qemu.wait(timeout=30)
        except subprocess.TimeoutExpired:
            qemu.kill()

    ok &= check("install task succeeded", bool(items) and all(t["state"] == "succeeded" for t in items),
                str([(t["state"], (t.get("error") or {}).get("code")) for t in items]))
    stages = items[0].get("stages", []) if items else []
    ok &= check("six stages all green",
                len(stages) == 6 and all(s["state"] == "succeeded" for s in stages),
                str([(s["name"], s["state"]) for s in stages]))
    _, jd = c.req("GET", f"/api/v1/jobs/{job_id}")
    ok &= check("job succeeded", jd.get("state") == "succeeded", jd.get("state", ""))

    if not all(t["state"] == "succeeded" for t in items):
        with open(serial_log) as f:
            print("---- serial tail ----")
            print("".join(f.readlines()[-60:]))
        return ok

    print("· boot the installed system from disk")
    boot_log = os.path.join(args.workdir, f"boot-{args.firmware}.log")
    bcmd = ["qemu-system-x86_64", "-m", "1024", "-smp", "2",
            "-display", "none", "-serial", f"file:{boot_log}",
            "-drive", f"file={disk},format=raw,if=none,id=disk0",
            "-device", "nvme,drive=disk0,serial=agentdisk",
            "-netdev", "user,id=n0,hostfwd=tcp:127.0.0.1:2222-:22",
            "-device", "virtio-net-pci,netdev=n0"]
    if args.firmware == "uefi":
        fw = "/opt/homebrew/share/qemu"
        vars2 = os.path.join(args.workdir, "vars2.fd")
        vars_src = os.path.join(fw, "edk2-x86_64-vars.fd")
        if not os.path.exists(vars_src):
            vars_src = os.path.join(fw, "edk2-i386-vars.fd")
        subprocess.run(["cp", vars_src, vars2], check=True)
        bcmd += ["-drive", f"if=pflash,format=raw,readonly=on,file={fw}/edk2-x86_64-code.fd",
                 "-drive", f"if=pflash,format=raw,file={vars2}"]
    with open(boot_log, "w") as bf:
        bq = subprocess.Popen(bcmd, stdout=bf, stderr=subprocess.STDOUT)

    def ssh_up():
        r = subprocess.run(
            ["ssh", "-i", key_path, "-p", "2222", "-o", "StrictHostKeyChecking=no",
             "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=5",
             "-o", "BatchMode=yes", "root@127.0.0.1", "hostname"],
            capture_output=True, text=True, timeout=15)
        return r.stdout.strip() or None

    try:
        hostname = wait_for(ssh_up, 300, "sshd on the installed system", interval=5)
    except TimeoutError as e:
        hostname = None
        print(f"  {FAIL} {e}")
        subprocess.run(["tail", "-40", boot_log])
    finally:
        bq.terminate()
        try:
            bq.wait(timeout=10)
        except subprocess.TimeoutExpired:
            bq.kill()

    want_host = f"agent-{args.firmware}-1"
    ok &= check(f"ssh into installed system, hostname={want_host}", hostname == want_host,
                repr(hostname))
    return ok


if __name__ == "__main__":
    sys.exit(0 if main() else 1)

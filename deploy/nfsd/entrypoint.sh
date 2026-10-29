#!/bin/sh
# mammoth-nfsd entrypoint — manage the host kernel nfsd for the media export.
# NFSv3 only (-N 2 drops v2, -V 3 keeps v3): the consumers are BMC virtual
# media and casper nfsroot, both v3; v4 is left to a dedicated deployment.
# Read-only export — the engine writes the media dir from its own bind mount.
set -eu

EXPORT_DIR="${MAMMOTH_NFS_EXPORT_DIR:-/export/media}"
EXPORT_OPTS="${MAMMOTH_NFS_EXPORT_OPTIONS:-*(ro,insecure,no_root_squash,no_subtree_check)}"
MOUNTD_PORT="${MAMMOTH_NFS_MOUNTD_PORT:-20048}"
THREADS="${MAMMOTH_NFS_THREADS:-8}"

[ -d "$EXPORT_DIR" ] || {
	echo "nfsd: export dir $EXPORT_DIR is empty — bind the media dir there (compose does this)" >&2
	exit 1
}

# nfsd is a loadable module on most distros; /lib/modules is bind-mounted ro
# when the host has it. Already-loaded kernels skip this.
modprobe nfsd 2>/dev/null || true

rpcbind -f -w &
RPCBIND_PID=$!

printf '%s %s\n' "$EXPORT_DIR" "$EXPORT_OPTS" > /etc/exports
exportfs -ra

# Starts the kernel threads (mounting /proc/fs/nfsd itself) and registers the
# nfs program with rpcbind for TCP+UDP. A failure here is a host-kernel
# problem, not a container one — say so before the raw error scrolls by.
if ! rpc.nfsd -N 2 -V 3 "$THREADS"; then
	echo "nfsd: rpc.nfsd failed — the host kernel has no nfsd support." >&2
	echo "      Docker Desktop VMs don't ship it: run nfs-server on the host instead" >&2
	echo "      (docs/operations.md §4.7)." >&2
	kill "$RPCBIND_PID" 2>/dev/null || true
	exit 1
fi

rpc.mountd -N 2 -V 3 --port "$MOUNTD_PORT" --foreground &
MOUNTD_PID=$!

echo "nfsd: exporting $EXPORT_DIR ($EXPORT_OPTS) — rpcbind 111, nfs 2049 tcp+udp, mountd $MOUNTD_PORT (v3 only)"

shutdown() {
	kill "$MOUNTD_PID" 2>/dev/null || true
	rpc.nfsd 0 2>/dev/null || true
	exportfs -ua 2>/dev/null || true
	kill "$RPCBIND_PID" 2>/dev/null || true
}
trap shutdown INT TERM

wait "$MOUNTD_PID"

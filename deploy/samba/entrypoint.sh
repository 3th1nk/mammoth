#!/bin/sh
# mammoth-samba entrypoint — guest read-only SMB2 export of the media dir.
# The consumer is WinPE's startnet: `net use Z: \\host\<share> "" /user:guest`
# (AllowInsecureGuestAuth is opted in client-side, windows.go startnet), then
# setup reads install.wim from the share. SMB1 is off (server min protocol =
# SMB2 — the WinPE clients here are Server 2019+); no NetBIOS (nmbd) — the
# client connects by IP. force user=root sidesteps host-side uid mismatches
# on the bind mount; the share is read-only so root access carries no write
# risk.
set -eu

SHARE_NAME="${MAMMOTH_SMB_SHARE_NAME:-mammoth-media}"
EXPORT_DIR="${MAMMOTH_SMB_EXPORT_DIR:-/export/media}"

[ -d "$EXPORT_DIR" ] || {
	echo "samba: export dir $EXPORT_DIR is empty — bind the media dir there (compose does this)" >&2
	exit 1
}

cat > /etc/samba/smb.conf <<EOF
[global]
	server role = standalone server
	server min protocol = SMB2
	map to guest = Bad User
	load printers = no
	disable spoolss = yes
	log file = /dev/stdout
	log level = 1

[$SHARE_NAME]
	path = $EXPORT_DIR
	guest ok = yes
	read only = yes
	browsable = yes
	force user = root
EOF

echo "samba: exporting $EXPORT_DIR as share '$SHARE_NAME' on :445 (SMB2+, guest read-only)"
exec smbd --foreground --no-process-group

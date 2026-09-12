#!/usr/bin/env python3
"""8MB raw disk with one MBR partition (LBA 2048, 8192 sectors) for the
probe qemu loop — exercises the /sys scan path (sda + sda1)."""
import struct
import sys

out = sys.argv[1] if len(sys.argv) > 1 else "/tmp/probe-testdisk.raw"
img = bytearray(8 * 1024 * 1024)
mbr = bytearray(512)
mbr[446:446+16] = struct.pack('<B3BB3BII',
                              0x80, 0xFE, 0xFF, 0xFF,   # bootable, dummy CHS
                              0x83, 0xFE, 0xFF, 0xFF,   # type Linux, dummy CHS
                              2048, 8192)               # LBA start, sectors
mbr[510:512] = b'\x55\xaa'
img[0:512] = mbr
with open(out, "wb") as f:
    f.write(img)
print(f"test disk written: {out}")

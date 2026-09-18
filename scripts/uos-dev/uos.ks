# UOS customized-anaconda Finish-crash reproduction kickstart
# (scripts/uos-dev/uos-qemu.sh; docs/compat/distros.md §uniontechos).
# Minimal shape that matches the real-machine automated run: full-disk
# rebuild on a blank disk, standard server package set (@core exists in
# every RHEL-lineage comps). The recorded crash is insensitive to
# kickstart content (bootloader/eula/user removals were ruled out there),
# so the package set does not need to match 489 packages exactly — the
# run just has to reach the Finish task group.
graphical
lang en_US.UTF-8
keyboard us
timezone Asia/Shanghai --utc
rootpw --plaintext UosQemu#2026
bootloader --location=mbr
clearpart --all --initlabel
autopart --type=plain
firstboot --disable
%packages
@core
%end
reboot

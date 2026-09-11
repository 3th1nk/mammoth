package render

import "regexp"

// kernelDevRe matches Linux block device names. Redfish inventories often
// report controller-level names instead ("LogicalDriveN") — those must be
// re-identified in %pre (kickstart) or matched by serial (curtin).
var kernelDevRe = regexp.MustCompile(
	`^(sd[a-z]+|nvme[0-9]+n[0-9]+|vd[a-z]+|hd[a-z]+|xvd[a-z]+|mmcblk[0-9]+|md[0-9]+|dm-[0-9]+)$`)

// IsKernelDeviceName reports whether dev looks like a Linux block device name.
func IsKernelDeviceName(dev string) bool { return kernelDevRe.MatchString(dev) }

package serve

import "golang.org/x/sys/unix"

func hostMemoryPressure() string {
	level, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	if err != nil {
		return "unknown"
	}
	switch level {
	case 1:
		return "normal"
	case 2:
		return "warn"
	case 4:
		return "critical"
	default:
		return "unknown"
	}
}

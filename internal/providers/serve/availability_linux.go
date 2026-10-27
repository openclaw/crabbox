package serve

import (
	"os"
	"strconv"
	"strings"
)

func hostMemoryPressure() string {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return "unknown"
	}
	values := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			if n, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				values[f[0]] = n
			}
		}
	}
	available, ok := values["MemAvailable:"]
	if !ok {
		return "unknown"
	}
	return memoryPressureFromAvailable(available, values["MemTotal:"])
}

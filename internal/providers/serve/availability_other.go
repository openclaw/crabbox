//go:build !darwin && !linux

package serve

func hostMemoryPressure() string { return "unknown" }

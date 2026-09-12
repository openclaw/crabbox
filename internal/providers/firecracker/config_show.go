package firecracker

import (
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection describes loaded values without inspecting guest assets.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.Firecracker
	return core.ProviderConfigShowSection{
		JSONKey: "firecracker", TextLabel: "firecracker", Providers: []string{"firecracker"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "binary", JSONValue: c.Binary, TextName: "binary", TextValue: core.Blank(c.Binary, "-")},
			{JSONName: "jailer", JSONValue: c.Jailer, TextName: "jailer", TextValue: core.Blank(c.Jailer, "-")},
			{JSONName: "kernel", JSONValue: c.Kernel, TextName: "kernel", TextValue: core.Blank(c.Kernel, "-")},
			{JSONName: "rootfs", JSONValue: c.RootFS, TextName: "rootfs", TextValue: core.Blank(c.RootFS, "-")},
			{JSONName: "user", JSONValue: c.User, TextName: "user", TextValue: core.Blank(c.User, "-")},
			{JSONName: "workRoot", JSONValue: c.WorkRoot, TextName: "work_root", TextValue: core.Blank(c.WorkRoot, "-")},
			{JSONName: "cpus", JSONValue: c.CPUs, TextName: "cpus", TextValue: strconv.Itoa(c.CPUs)},
			{JSONName: "memoryMiB", JSONValue: c.MemoryMiB, TextName: "memory_mib", TextValue: strconv.Itoa(c.MemoryMiB)},
			{JSONName: "diskMiB", JSONValue: c.DiskMiB, TextName: "disk_mib", TextValue: strconv.Itoa(c.DiskMiB)},
			{JSONName: "network", JSONValue: c.Network, TextName: "network", TextValue: core.Blank(c.Network, "-")},
			{JSONName: "cniNetwork", JSONValue: c.CNINetwork, TextName: "cni_network", TextValue: core.Blank(c.CNINetwork, "-")},
			{JSONName: "cniConfDir", JSONValue: c.CNIConfDir, TextName: "cni_conf_dir", TextValue: core.Blank(c.CNIConfDir, "-")},
			{JSONName: "cniBinDir", JSONValue: c.CNIBinDir, TextName: "cni_bin_dir", TextValue: core.Blank(c.CNIBinDir, "-")},
			{JSONName: "launchTimeout", JSONValue: c.LaunchTimeout.String(), TextName: "launch_timeout", TextValue: c.LaunchTimeout.String()},
			{JSONName: "deleteOnRelease", JSONValue: c.DeleteOnRelease, TextName: "delete_on_release", TextValue: strconv.FormatBool(c.DeleteOnRelease)},
		},
	}
}

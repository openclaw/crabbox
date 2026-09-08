package cli

//go:generate go run ../../scripts/configgen -source config_smolvm.go -output config_smolvm_generated.go -type SmolvmConfig -provider smolvm

// SmolvmConfig describes mechanical bindings for the delegated provider.
// Credential provenance and destination validation remain owned by core policy.
type SmolvmConfig struct {
	APIKey   string `env:"CRABBOX_SMOLVM_API_KEY" envAlias:"SMOLMACHINES_API_KEY" envAlias2:"SMK_API_KEY" sources:"env" reportApplied:"true"`
	BaseURL  string `config:"baseUrl" env:"CRABBOX_SMOLVM_BASE_URL" flag:"smolvm-base-url" sources:"user,repo,env,flag" help:"SmolVM / SmolFleet API base URL" default:"https://api.smolmachines.com" fileIgnoreEmpty:"true" reportApplied:"true"`
	Image    string `config:"image" env:"CRABBOX_SMOLVM_IMAGE" flag:"smolvm-image" sources:"user,repo,env,flag" help:"source image for smolvm machines (e.g. ubuntu:24.04)" default:"alpine" fileIgnoreEmpty:"true"`
	Workdir  string `config:"workdir" env:"CRABBOX_SMOLVM_WORKDIR" flag:"smolvm-workdir" sources:"user,repo,env,flag" help:"absolute working directory inside the smolvm machine" default:"/workspace" fileIgnoreEmpty:"true"`
	CPUs     int    `config:"cpus" env:"CRABBOX_SMOLVM_CPUS" flag:"smolvm-cpus" sources:"user,repo,env,flag" help:"number of vCPUs for the smolvm machine" default:"2" nonnegative:"true" fileInt:"positive" envInt:"fallback"`
	MemoryMB int    `config:"memoryMB" env:"CRABBOX_SMOLVM_MEMORY_MB" flag:"smolvm-memory-mb" sources:"user,repo,env,flag" help:"memory in MiB for the smolvm machine" default:"2048" nonnegative:"true" fileInt:"positive" envInt:"fallback"`
	Network  string `config:"network" env:"CRABBOX_SMOLVM_NETWORK" flag:"smolvm-network" sources:"user,repo,env,flag" help:"network mode: open or blocked" default:"open" fileIgnoreEmpty:"true"`
	Keep     bool   `config:"keep" env:"CRABBOX_SMOLVM_KEEP" flag:"smolvm-keep" sources:"user,repo,env,flag" help:"keep the smolvm machine after run (do not auto-delete)"`
}

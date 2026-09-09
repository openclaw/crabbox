package cli

//go:generate go run ../../scripts/configgen -source config_opencomputer.go -output config_opencomputer_generated.go -type OpenComputerConfig -provider opencomputer

// OpenComputerConfig describes mechanical bindings for the delegated provider.
// APIURL stays unset by default so external OC config precedes the client's
// built-in URL. API-key resolution remains outside this config structure.
type OpenComputerConfig struct {
	APIURL          string `env:"CRABBOX_OPENCOMPUTER_API_URL" envAlias:"OPENCOMPUTER_API_URL" flag:"opencomputer-api-url" sources:"env,flag" help:"Trusted OpenComputer API base URL; not accepted from repository config"`
	Workdir         string `config:"workdir" env:"CRABBOX_OPENCOMPUTER_WORKDIR" flag:"opencomputer-workdir" sources:"user,repo,env,flag" help:"Absolute working directory inside the sandbox (also used as sync target)" default:"/workspace/crabbox" fileIgnoreEmpty:"true"`
	CPU             int    `config:"cpu" env:"CRABBOX_OPENCOMPUTER_CPU" flag:"opencomputer-cpu" sources:"user,repo,env,flag" help:"OpenComputer sandbox vCPU count (0 = service default; the service infers memory when omitted)" nonnegative:"true" fileInt:"present" envInt:"fallback"`
	MemoryMB        int    `config:"memoryMB" env:"CRABBOX_OPENCOMPUTER_MEMORY_MB" flag:"opencomputer-memory-mb" sources:"user,repo,env,flag" help:"OpenComputer sandbox memory in MB (0 = service default; the service infers CPU when omitted)" nonnegative:"true" fileInt:"present" envInt:"fallback"`
	TimeoutSecs     int    `config:"timeoutSecs" env:"CRABBOX_OPENCOMPUTER_TIMEOUT_SECS" flag:"opencomputer-timeout-secs" sources:"user,repo,env,flag" help:"OpenComputer sandbox idle timeout in seconds (0 = service default)" nonnegative:"true" fileInt:"present" envInt:"fallback"`
	ExecTimeoutSecs int    `config:"execTimeoutSecs" env:"CRABBOX_OPENCOMPUTER_EXEC_TIMEOUT_SECS" flag:"opencomputer-exec-timeout-secs" sources:"user,repo,env,flag" help:"OpenComputer command timeout in seconds (0 = Crabbox default 3600)" default:"3600" nonnegative:"true" fileInt:"present" envInt:"fallback"`
	Burst           bool   `config:"burst" env:"CRABBOX_OPENCOMPUTER_BURST" flag:"opencomputer-burst" sources:"user,repo,env,flag" help:"use alpha best-effort burst capacity; filesystem persists but processes may restart"`
	ForgetMissing   bool   `flag:"opencomputer-forget-missing" sources:"flag" help:"remove the local claim when stop gets 404 (explicit stale-claim cleanup)"`
}

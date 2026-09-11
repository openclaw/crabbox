package cli

//go:generate go run ../../scripts/configgen -source config_freestyle.go -output config_freestyle_generated.go -type FreestyleConfig -provider freestyle

// FreestyleConfig owns mechanical inputs; provider validation remains separate.
type FreestyleConfig struct {
	APIKey   string `env:"CRABBOX_FREESTYLE_API_KEY" envAlias:"FREESTYLE_API_KEY" sources:"env"`
	APIURL   string `config:"apiUrl" env:"CRABBOX_FREESTYLE_API_URL" envAlias:"FREESTYLE_API_URL" flag:"freestyle-api-url" sources:"user,env,flag" help:"Freestyle API URL" default:"https://api.freestyle.sh" fileIgnoreEmpty:"true" fileStorage:"value"`
	Workdir  string `config:"workdir" env:"CRABBOX_FREESTYLE_WORKDIR" flag:"freestyle-workdir" sources:"user,repo,env,flag" help:"Freestyle sandbox workdir" default:"crabbox" fileIgnoreEmpty:"true" fileStorage:"value"`
	VCPUs    int    `config:"vcpus" env:"CRABBOX_FREESTYLE_VCPUS" flag:"freestyle-vcpus" sources:"user,repo,env,flag" help:"Freestyle sandbox vCPUs (power of two; omit for plan default)" nonnegative:"true" fileInt:"positive" envInt:"fallback" fileStorage:"value"`
	MemoryGB int    `config:"memoryGB" env:"CRABBOX_FREESTYLE_MEMORY_GB" flag:"freestyle-memory-gb" sources:"user,repo,env,flag" help:"Freestyle sandbox memory in GiB (power of two; omit for plan default)" nonnegative:"true" fileInt:"positive" envInt:"fallback" fileStorage:"value"`
}

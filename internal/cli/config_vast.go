package cli

//go:generate go run ../../scripts/configgen -source config_vast.go -output config_vast_generated.go -type VastConfig -provider vast

// VastConfig contains Vast.ai provider settings. APIKey is populated only from
// CRABBOX_VAST_API_KEY / VAST_API_KEY and must not be persisted or printed.
type VastConfig struct {
	APIKey         string  `env:"CRABBOX_VAST_API_KEY" envAlias:"VAST_API_KEY" sources:"env" reportApplied:"true"`
	APIURL         string  `config:"apiUrl" env:"CRABBOX_VAST_API_URL" envAlias:"VAST_API_URL" flag:"vast-api-url" sources:"user,repo,env,flag" help:"Vast.ai REST API URL" default:"https://console.vast.ai/api/v0" fileIgnoreEmpty:"true" reportApplied:"true"`
	InstanceType   string  `config:"instanceType" env:"CRABBOX_VAST_INSTANCE_TYPE" flag:"vast-instance-type" sources:"user,repo,env,flag" help:"Vast.ai offer type: ondemand or interruptible" default:"ondemand" fileIgnoreEmpty:"true" reportApplied:"true"`
	GPUName        string  `config:"gpuName" env:"CRABBOX_VAST_GPU_NAME" flag:"vast-gpu-name" sources:"user,repo,env,flag" help:"Vast.ai GPU name selector" fileIgnoreEmpty:"true"`
	GPUCount       int     `config:"gpuCount" env:"CRABBOX_VAST_GPU_COUNT" flag:"vast-gpu-count" sources:"user,repo,env,flag" help:"Vast.ai minimum GPU count" nonnegative:"true" fileInt:"nonzero" envInt:"fallback"`
	Image          string  `config:"image" env:"CRABBOX_VAST_IMAGE" flag:"vast-image" sources:"user,repo,env,flag" help:"Docker image to deploy on the instance" default:"nvidia/cuda:12.8.1-cudnn-devel-ubuntu22.04" fileIgnoreEmpty:"true"`
	TemplateID     string  `config:"templateId" env:"CRABBOX_VAST_TEMPLATE_ID" flag:"vast-template-id" sources:"user,repo,env,flag" help:"Optional Vast.ai template ID" fileIgnoreEmpty:"true"`
	Runtype        string  `config:"runtype" env:"CRABBOX_VAST_RUNTYPE" flag:"vast-runtype" sources:"user,repo,env,flag" help:"Vast.ai runtime type: ssh_direct" default:"ssh_direct" fileIgnoreEmpty:"true"`
	DiskGB         int     `config:"diskGB" env:"CRABBOX_VAST_DISK_GB" flag:"vast-disk-gb" sources:"user,repo,env,flag" help:"Instance disk size in GB" default:"20" nonnegative:"true" fileInt:"nonzero" envInt:"fallback"`
	MaxDphTotal    float64 `config:"maxDphTotal" env:"CRABBOX_VAST_MAX_DPH_TOTAL" flag:"vast-max-dph-total" sources:"user,repo,env,flag" help:"Maximum total dollars per hour"`
	MinReliability float64 `config:"minReliability" env:"CRABBOX_VAST_MIN_RELIABILITY" flag:"vast-min-reliability" sources:"user,repo,env,flag" help:"Minimum reliability score from 0 to 1"`
	Order          string  `config:"order" env:"CRABBOX_VAST_ORDER" flag:"vast-order" sources:"user,repo,env,flag" help:"Vast.ai offer ordering expression" default:"dlperf_per_dphtotal desc" fileIgnoreEmpty:"true"`
	User           string  `config:"user" env:"CRABBOX_VAST_USER" flag:"vast-user" sources:"user,repo,env,flag" help:"SSH user for Vast.ai instances" default:"root" fileIgnoreEmpty:"true"`
	WorkRoot       string  `config:"workRoot" env:"CRABBOX_VAST_WORK_ROOT" flag:"vast-work-root" sources:"user,repo,env,flag" help:"remote Crabbox work root on Vast.ai instances" default:"/work/crabbox" fileIgnoreEmpty:"true" reportApplied:"true"`
	ReleaseAction  string  `config:"releaseAction" env:"CRABBOX_VAST_RELEASE_ACTION" flag:"vast-release-action" sources:"user,repo,env,flag" help:"Vast.ai release action: destroy, stop, or keep" default:"destroy" fileIgnoreEmpty:"true" reportApplied:"true"`
}

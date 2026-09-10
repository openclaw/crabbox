package cli

//go:generate go run ../../scripts/configgen -source config_runpod.go -output config_runpod_generated.go -type RunpodConfig -provider runpod

// These runtime fallbacks do not initialize the raw User or WorkRoot fields.
const (
	RunpodSSHUserFallback  = "root"
	RunpodWorkRootFallback = "/tmp/crabbox"
)

// RunpodConfig describes mechanical bindings; credential provenance and
// provider runtime defaulting remain separate policy owners.
type RunpodConfig struct {
	APIKey     string `env:"CRABBOX_RUNPOD_API_KEY" envAlias:"RUNPOD_API_KEY" sources:"env" reportApplied:"true"`
	APIURL     string `config:"apiUrl" env:"CRABBOX_RUNPOD_API_URL" envAlias:"RUNPOD_API_URL" flag:"runpod-url" sources:"user,repo,env,flag" help:"RunPod REST API URL" default:"https://rest.runpod.io/v1" fileIgnoreEmpty:"true" reportApplied:"true" fileStorage:"value"`
	CloudType  string `config:"cloudType" env:"CRABBOX_RUNPOD_CLOUD_TYPE" envAlias:"RUNPOD_CLOUD_TYPE" flag:"runpod-cloud-type" sources:"user,repo,env,flag" help:"RunPod cloud type: SECURE or COMMUNITY" default:"SECURE" fileIgnoreEmpty:"true" fileStorage:"value"`
	InstanceID string `config:"instanceId" env:"CRABBOX_RUNPOD_INSTANCE_ID" envAlias:"RUNPOD_INSTANCE_ID" flag:"runpod-instance-id" sources:"user,repo,env,flag" help:"RunPod GPU type ID or CPU flavor ID" default:"NVIDIA L4,NVIDIA RTX 4000 Ada Generation,NVIDIA RTX A4000,NVIDIA GeForce RTX 3090,NVIDIA GeForce RTX 4090,NVIDIA RTX A5000,NVIDIA RTX A4500" fileIgnoreEmpty:"true" fileStorage:"value"`
	Image      string `config:"image" env:"CRABBOX_RUNPOD_IMAGE" envAlias:"RUNPOD_IMAGE" flag:"runpod-image" sources:"user,repo,env,flag" help:"Docker image to deploy on the pod" default:"runpod/pytorch:2.8.0-py3.11-cuda12.8.1-cudnn-devel-ubuntu22.04" fileIgnoreEmpty:"true" fileStorage:"value"`
	TemplateID string `config:"templateId" env:"CRABBOX_RUNPOD_TEMPLATE_ID" envAlias:"RUNPOD_TEMPLATE_ID" flag:"runpod-template-id" sources:"user,repo,env,flag" help:"Optional RunPod template ID" fileIgnoreEmpty:"true" fileStorage:"value"`
	DiskGB     int    `config:"diskGB" env:"CRABBOX_RUNPOD_DISK_GB" flag:"runpod-disk-gb" sources:"user,repo,env,flag" help:"Container disk size in GB" default:"20" nonnegative:"true" fileInt:"nonzero" envInt:"fallback" fileStorage:"value"`
	User       string `config:"user" env:"CRABBOX_RUNPOD_USER" flag:"runpod-user" sources:"user,repo,env,flag" help:"SSH user for runpod pods" fileIgnoreEmpty:"true" fileStorage:"value"`
	WorkRoot   string `config:"workRoot" env:"CRABBOX_RUNPOD_WORK_ROOT" flag:"runpod-work-root" sources:"user,repo,env,flag" help:"remote Crabbox work root on runpod pods" fileIgnoreEmpty:"true" fileStorage:"value"`
}

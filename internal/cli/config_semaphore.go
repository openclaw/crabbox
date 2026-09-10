package cli

//go:generate go run ../../scripts/configgen -source config_semaphore.go -output config_semaphore_generated.go -type SemaphoreConfig -provider semaphore

// SemaphoreConfig describes mechanical bindings for the SSH lease provider.
// Effective flag/runtime fallbacks do not populate the raw configuration.
type SemaphoreConfig struct {
	Host        string `config:"host" env:"CRABBOX_SEMAPHORE_HOST" envAlias:"SEMAPHORE_HOST" flag:"semaphore-host" sources:"user,repo,env,flag" help:"Semaphore host (e.g. myorg.semaphoreci.com)" fileIgnoreEmpty:"true" reportApplied:"true" fileStorage:"value"`
	Token       string `config:"token" env:"CRABBOX_SEMAPHORE_TOKEN" envAlias:"SEMAPHORE_API_TOKEN" sources:"user,repo,env" fileIgnoreEmpty:"true" reportApplied:"true" fileStorage:"value"`
	Project     string `config:"project" env:"CRABBOX_SEMAPHORE_PROJECT" envAlias:"SEMAPHORE_PROJECT" flag:"semaphore-project" sources:"user,repo,env,flag" help:"Semaphore project name" fileIgnoreEmpty:"true" fileStorage:"value"`
	Machine     string `config:"machine" env:"CRABBOX_SEMAPHORE_MACHINE" flag:"semaphore-machine" sources:"user,repo,env,flag" help:"Machine type" flagFallback:"f1-standard-2" fileIgnoreEmpty:"true" fileStorage:"value"`
	OSImage     string `config:"osImage" env:"CRABBOX_SEMAPHORE_OS_IMAGE" flag:"semaphore-os-image" sources:"user,repo,env,flag" help:"OS image" flagFallback:"ubuntu2204" fileIgnoreEmpty:"true" fileStorage:"value"`
	IdleTimeout string `config:"idleTimeout" env:"CRABBOX_SEMAPHORE_IDLE_TIMEOUT" flag:"semaphore-idle-timeout" sources:"user,repo,env,flag" help:"Idle timeout" flagFallback:"30m" fileIgnoreEmpty:"true" fileStorage:"value"`
}

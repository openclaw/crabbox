package cli

import "time"

//go:generate go run ../../scripts/configgen -source config_namespace.go -output config_namespace_generated.go -type NamespaceConfig -provider namespace

// NamespaceConfig owns Devbox bindings; generic flag effects stay with the provider.
type NamespaceConfig struct {
	Image               string        `config:"image" env:"CRABBOX_NAMESPACE_IMAGE" flag:"namespace-image" sources:"user,repo,env,flag" help:"Namespace Devbox image" default:"builtin:base" fileIgnoreEmpty:"true" fileStorage:"value"`
	Size                string        `config:"size" env:"CRABBOX_NAMESPACE_SIZE" flag:"namespace-size" sources:"user,repo,env,flag" help:"Namespace Devbox size: S, M, L, or XL" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Repository          string        `config:"repository" env:"CRABBOX_NAMESPACE_REPOSITORY" flag:"namespace-repository" sources:"user,repo,env,flag" help:"Namespace Devbox repository checkout" fileIgnoreEmpty:"true" fileStorage:"value"`
	Site                string        `config:"site" env:"CRABBOX_NAMESPACE_SITE" flag:"namespace-site" sources:"user,repo,env,flag" help:"Namespace Devbox site" fileIgnoreEmpty:"true" fileStorage:"value"`
	VolumeSizeGB        int           `config:"volumeSizeGB" env:"CRABBOX_NAMESPACE_VOLUME_SIZE_GB" flag:"namespace-volume-size-gb" sources:"user,repo,env,flag" help:"Namespace Devbox persistent volume size in GiB" nonnegative:"true" fileInt:"positive" envInt:"fallback" fileStorage:"value"`
	AutoStopIdleTimeout time.Duration `config:"autoStopIdleTimeout" env:"CRABBOX_NAMESPACE_AUTO_STOP_IDLE_TIMEOUT" flag:"namespace-auto-stop-idle-timeout" sources:"user,repo,env,flag" help:"Namespace Devbox idle auto-stop timeout" default:"30m" duration:"positive-overlay" fileStorage:"value" flagDuration:"trim-positive" flagDurationError:"namespace auto-stop idle timeout must be a positive duration"`
	WorkRoot            string        `config:"workRoot" env:"CRABBOX_NAMESPACE_WORK_ROOT" flag:"namespace-work-root" sources:"user,repo,env,flag" help:"Namespace Devbox Crabbox work root" default:"/workspaces/crabbox" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	DeleteOnRelease     bool          `config:"deleteOnRelease" env:"CRABBOX_NAMESPACE_DELETE_ON_RELEASE" flag:"namespace-delete-on-release" sources:"user,repo,env,flag" help:"delete Namespace Devbox on release instead of shutting it down" reportApplied:"true"`
}

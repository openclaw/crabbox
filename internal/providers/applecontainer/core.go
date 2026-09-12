package applecontainer

import (
	core "github.com/openclaw/crabbox/internal/cli"
)


const (
	providerName = "apple-container"
	targetLinux  = core.TargetLinux
	// sshPort is the in-guest SSH port. Apple's container runtime gives each
	// container a routable IP on the host vmnet bridge, so unlike Docker we
	// connect straight to the container IP on the standard SSH port rather
	// than to a published host port.
	sshPort            = "22"
	workRootMarkerName = ".crabbox-apple-container-work-root"
)

package dockersandbox

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName   = "docker-sandbox"
	leasePrefix    = "dsbx_"
	namePrefix     = "crabbox-"
	defaultCLIPath = "sbx"
	defaultAgent   = "shell"
	baselineSBX    = "v0.31.3"
	defaultWorkdir = "/workspace"
	targetLinux    = core.TargetLinux
	NetworkPublic  = core.NetworkPublic

	maxSandboxNameLen    = 63
	sandboxNameSuffixLen = 6
)

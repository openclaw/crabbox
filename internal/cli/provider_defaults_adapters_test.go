package cli_test

// Register the real defaults owners so core configuration tests exercise adapter hooks.
import (
	_ "github.com/openclaw/crabbox/internal/providers/digitalocean"
	_ "github.com/openclaw/crabbox/internal/providers/lambda"
	_ "github.com/openclaw/crabbox/internal/providers/linode"
	_ "github.com/openclaw/crabbox/internal/providers/nebius"
	_ "github.com/openclaw/crabbox/internal/providers/ovh"
	_ "github.com/openclaw/crabbox/internal/providers/scaleway"
	_ "github.com/openclaw/crabbox/internal/providers/tencentcloud"
	_ "github.com/openclaw/crabbox/internal/providers/vultr"
)

package cli_test

// Register the real defaults owners so core configuration tests exercise adapter hooks.
import (
	_ "github.com/openclaw/crabbox/internal/providers/digitalocean"
	_ "github.com/openclaw/crabbox/internal/providers/linode"
)

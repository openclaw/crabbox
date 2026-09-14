package cli

import "maps"

// This is the canonical loader's audited input-owner roster, not the provider
// registry. New providers remain unknown until their source handling is covered.
var canonicalConfigInputOwners = [...]configInputOwner{
	"agent-sandbox",
	"anthropic-sandbox-runtime",
	"apple-container",
	"apple-machine",
	"apple-vm",
	"ascii-box",
	"aws",
	"aws-lambda-microvm",
	"azure",
	"azure-dynamic-sessions",
	"blacksmith-testbox",
	"blaxel",
	"boxd",
	"cloud-run-sandbox",
	"cloudflare",
	"cloudflare-dynamic-workers",
	"cloudflare-sandbox",
	"coder",
	"codesandbox",
	"crownest",
	"cua",
	"cubesandbox",
	"daytona",
	"digitalocean",
	"docker-sandbox",
	"e2b",
	"exe-dev",
	"external",
	"fastapi-cloud",
	"firecracker",
	"freestyle",
	"gcp",
	"github-codespaces",
	"hetzner",
	"hostinger",
	"hyperv",
	"incus",
	"islo",
	"kubevirt",
	"lambda",
	"linode",
	"local-container",
	"lume",
	"machine0",
	"modal",
	"morph",
	"multipass",
	"mxc",
	"namespace-devbox",
	"namespace-instance",
	"nebius",
	"nomad",
	"nvidia-brev",
	"opencomputer",
	"opensandbox",
	"orgo",
	"ovh",
	"parallels",
	"phala",
	"proxmox",
	"railway",
	"runpod",
	"scaleway",
	"sealos-devbox",
	"semaphore",
	"smolvm",
	"sprites",
	"ssh",
	"superserve",
	"tart",
	"tencentcloud",
	"tenki",
	"tensorlake",
	"unikraft-cloud",
	"upstash-box",
	"vast",
	"vercel-sandbox",
	"vultr",
	"wandb",
	"windows-sandbox",
	"xcp-ng",
}

// Call only after a successful canonical load has accounted for every file and
// environment layer. Defaults, partial overlays and restored snapshots must not
// acquire a completeness claim merely by being represented as Config.
func completeCanonicalConfigInputs(cfg *Config) {
	if cfg.synthesizedFlagInputs {
		return
	}
	ledger := maps.Clone(cfg.inputProvenance)
	if ledger == nil {
		ledger = make(configInputLedger)
	}
	for _, owner := range canonicalConfigInputOwners {
		facts := ledger[owner]
		facts.complete = true
		ledger[owner] = facts
	}
	facts := ledger[configInputGeneric]
	facts.complete = true
	ledger[configInputGeneric] = facts
	cfg.inputProvenance = ledger
}

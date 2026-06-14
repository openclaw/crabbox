package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

type App struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
}

func Run(ctx context.Context, args []string) error {
	app := App{Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin}
	return app.Run(ctx, args)
}

func (a App) input() io.Reader {
	if a.Stdin != nil {
		return a.Stdin
	}
	return os.Stdin
}

func (a App) Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		a.printHelp()
		return exit(2, "missing command")
	}

	switch args[0] {
	case "__namespace-instance-proxy":
		return a.namespaceInstanceProxy(ctx, args[1:])
	case "-h", "--help":
		a.printHelp()
		return nil
	case "help":
		if len(args) > 1 {
			return a.runKong(ctx, args)
		}
		a.printHelp()
		return nil
	}
	if help, ok := a.directCommandHelp(ctx, args); ok {
		return help
	}

	return a.runKong(ctx, args)
}

func (a App) directCommandHelp(ctx context.Context, args []string) (error, bool) {
	if len(args) < 2 || !isHelpArg(args[1]) || isKongCommandGroup(args[0]) {
		return nil, false
	}
	helpArgs := []string{"--help"}
	switch args[0] {
	case "init":
		return a.initProject(ctx, helpArgs), true
	case "login":
		return a.login(ctx, helpArgs), true
	case "logout":
		return a.logout(ctx, helpArgs), true
	case "whoami":
		return a.whoami(ctx, helpArgs), true
	case "doctor":
		return a.doctor(ctx, helpArgs), true
	case "warmup":
		return a.warmup(ctx, helpArgs), true
	case "prewarm":
		return a.prewarm(ctx, helpArgs), true
	case "run":
		return a.runCommand(ctx, helpArgs), true
	case "job":
		return nil, false
	case "sync-plan":
		return a.syncPlan(ctx, helpArgs), true
	case "providers":
		return a.providers(ctx, helpArgs), true
	case "history":
		return a.history(ctx, helpArgs), true
	case "logs":
		return a.logs(ctx, helpArgs), true
	case "events":
		return a.events(ctx, helpArgs), true
	case "attach":
		return a.attach(ctx, helpArgs), true
	case "results":
		return a.results(ctx, helpArgs), true
	case "status":
		return a.status(ctx, helpArgs), true
	case "list":
		return a.list(ctx, helpArgs), true
	case "usage":
		return a.usage(ctx, helpArgs), true
	case "ssh":
		return a.ssh(ctx, helpArgs), true
	case "ports":
		return a.ports(ctx, helpArgs), true
	case "cp":
		return a.copyCommand(ctx, helpArgs), true
	case "vnc":
		return a.vnc(ctx, helpArgs), true
	case "webvnc":
		return a.webvnc(ctx, helpArgs), true
	case "code":
		return a.webCode(ctx, helpArgs), true
	case "egress":
		return a.egress(ctx, helpArgs), true
	case "screenshot":
		return a.screenshot(ctx, helpArgs), true
	case "artifacts":
		return nil, false
	case "capsule":
		return nil, false
	case "checkpoint":
		return nil, false
	case "inspect":
		return a.inspect(ctx, helpArgs), true
	case "stop", "release":
		return a.stop(ctx, helpArgs), true
	case "pause":
		return a.pause(ctx, helpArgs), true
	case "resume":
		return a.resume(ctx, helpArgs), true
	case "cleanup":
		return a.cleanup(ctx, helpArgs), true
	default:
		return nil, false
	}
}

func isHelpArg(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "help"
}

func (a App) printHelp() {
	fmt.Fprintf(a.Stdout, `Crabbox leases remote test boxes, syncs your dirty checkout, runs commands, and cleans up.

Usage:
  crabbox <command> [flags]
  crabbox run [flags] -- <command...>

Start Here:
  crabbox login --url https://broker.example.com
      Open GitHub login for your broker and store credentials.
  crabbox doctor
      Check local tools, config, broker, and provider access.
  crabbox init
      Add repo-local Crabbox config, GitHub workflow, and agent skill.
  crabbox warmup --class beast
      Lease a reusable box and print a cbx_... id plus friendly slug.
  crabbox prewarm
      Lease a reusable box and hydrate it from configured GitHub Actions.
  crabbox run --id blue-lobster -- pnpm test:changed
      Sync this checkout to the box and run a command.
  crabbox warmup --desktop --browser --code
      Lease a UI-capable box with a browser and web code editor.

Commands:
  init        Onboard the current repo for Crabbox
  login       Open GitHub login, store broker credentials, verify access
  logout      Remove the stored broker token
  whoami      Show broker identity
  doctor      Check local and broker/provider readiness
  warmup      Lease a box and wait until it is ready
  prewarm     Lease and hydrate a reusable test-ready box
  run         Sync the repo, run a remote command, stream output
  job         Run named repo-local Crabbox jobs
  desktop     Launch apps into a visible desktop session
  media       Create preview artifacts from recorded desktop videos
  artifacts   Collect, transform, and publish QA artifacts
  sync-plan   Show local sync manifest size hotspots
  providers   Show provider capabilities
  history     List recorded remote runs
  logs        Print recorded run logs
  events      Print recorded run events
  attach      Follow recorded events for an active run
  results     Show recorded test result summaries
  cache       Inspect, purge, warm, or list remote cache volumes
  status      Show lease state; add --wait to block until ready
  list        List Crabbox machines
  share       Share a lease with users or the owning org
  unshare     Remove lease sharing
  image       Create provider images and promote brokered AWS runner images
  usage       Show cost and usage estimates by user, org, or fleet
  admin       Lease admin controls for trusted operators
  actions     Hydrate boxes from repo workflows or GitHub runners
  capsule     Capture and replay lightweight failure capsules
  checkpoint  Create, restore, and fork workspace checkpoints
  ssh         Print the SSH command for a lease
  ports       Publish, list, or unpublish provider-native ports
  cp          Copy files between host and a delegated sandbox
  vnc         Print or open VNC connection details for a desktop lease
  webvnc      Open a desktop lease or local VNC tunnel in a browser
  code        Bridge a code lease into the authenticated web portal
  egress      Bridge lease browser/app traffic through this machine
  screenshot  Capture a PNG from a desktop lease
  inspect     Print lease/provider details; add --json for scripts
  stop        Release a lease or delete a direct-provider machine
  pause       Pause a lease, freeing remote compute while preserving state
  resume      Resume a previously paused lease
  cleanup     Sweep expired direct-provider machines or local provider state
  pool        Manage ready-pool leases and list machine inventory aliases
  pond        Bridge plane peer discovery for delegated providers
  azure       Azure provider setup and login
  config      Show or update user config
  adapter     Serve or connect the Crabfleet runtime adapter

Common Flows:
  crabbox run --class beast -- pnpm check
  crabbox job run openclaw-wsl2
  crabbox warmup
  crabbox status --id blue-lobster --wait
  crabbox run --id blue-lobster --shell 'pnpm install --frozen-lockfile && pnpm test'
  crabbox ssh --id blue-lobster
  crabbox ports --id blue-lobster --publish 8080
  crabbox cp --id blue-lobster ./coverage.xml SANDBOX:/tmp/coverage.xml
  crabbox vnc --id blue-lobster --open
  crabbox desktop launch --id blue-lobster --browser --url https://example.com --webvnc --open
  crabbox desktop proof --id blue-lobster --output artifacts/blue-lobster-proof -- ./scripts/visual-smoke.sh
  crabbox media preview --input desktop.mp4 --output desktop-preview.gif --trimmed-video-output desktop-change.mp4
  crabbox artifacts collect --id blue-lobster --all --output artifacts/blue-lobster
  crabbox artifacts publish --pr 123 --dir artifacts/blue-lobster --storage s3 --bucket qa-artifacts
  crabbox artifacts list artifacts/blue-lobster
  crabbox artifacts pull artifacts/blue-lobster --output /tmp/blue-lobster-proof
  crabbox providers
  crabbox providers --json
  crabbox webvnc --id blue-lobster --open
  crabbox code --id blue-lobster --open
  crabbox egress start --id blue-lobster --profile discord --daemon
  crabbox share --id blue-lobster --user friend@example.com
  crabbox share --id blue-lobster --org
  crabbox screenshot --id blue-lobster --output desktop.png
  crabbox inspect --id blue-lobster --json
  crabbox history --lease cbx_abcdef123456
  crabbox logs run_123
  crabbox events run_123
  crabbox attach run_123
  crabbox results run_123
  crabbox cache stats --id blue-lobster
  crabbox cache volumes
  crabbox pool ready
  crabbox usage --scope org
  crabbox admin leases --state active
  crabbox admin lease-audit --state expired --provider aws
  crabbox admin providers identity --provider aws --region eu-west-1
  crabbox admin providers policy --provider aws --target macos
  crabbox admin hosts policy --provider aws --target macos
  crabbox admin hosts offerings --provider aws --target macos --region eu-west-1
  crabbox admin hosts quota --provider aws --target macos --region eu-west-1 --type mac2.metal
  crabbox admin hosts list --provider aws --target macos --region eu-west-1
  crabbox admin hosts allocate --provider aws --target macos --region eu-west-1 --dry-run
  crabbox warmup --actions-runner
  crabbox actions hydrate --id blue-lobster
  crabbox actions dispatch -f testbox_id=cbx_abcdef123456
  crabbox capsule from-actions https://github.com/example-org/my-app/actions/runs/123 --replay 'go test ./...'
  crabbox capsule replay capsules/example-org-my-app-actions-123/capsule.yaml --keep
  crabbox checkpoint create --id blue-lobster --name after-install --mode native
  crabbox checkpoint fork chk_abcdef1234567890 --class beast
  crabbox run --provider ssh --target macos --static-host mac.local -- echo ok
  crabbox run --provider ssh --target windows --windows-mode normal --static-host win.local -- pwsh -NoProfile -Command '$PSVersionTable'
  crabbox stop blue-lobster
  crabbox adapter connect --id mac-lab --local-socket "$HOME/.local/run/crabbox/adapter.sock" --token-file ~/.config/crabbox/adapter.token
  crabbox adapter serve --unix-socket "$HOME/.local/run/crabbox/adapter.sock" --token-file ~/.config/crabbox/adapter.token --provider external

Global:
  -h, --help     Show help
  --version      Print version

Config:
  crabbox login --url <url> [--provider aws|azure|hetzner] [--no-browser]
  crabbox login --url <url> --token-stdin [--provider aws|azure|hetzner]
  crabbox azure login [--subscription <id>] [--location <loc>] [--json]
  crabbox config path
  crabbox config show [--json]
  crabbox config set-broker --url <url> --token-stdin [--provider aws|azure|hetzner]

Environment:
  CRABBOX_COORDINATOR          Broker URL
  CRABBOX_COORDINATOR_TOKEN    Broker bearer token
  CRABBOX_COORDINATOR_TOKEN_COMMAND
                               JSON argv array that prints a fresh broker bearer token
  CRABBOX_COORDINATOR_ADMIN_TOKEN
                               Broker admin bearer token
  CRABBOX_ACCESS_CLIENT_ID     Cloudflare Access service token client ID
  CRABBOX_ACCESS_CLIENT_SECRET Cloudflare Access service token client secret
  CRABBOX_ACCESS_TOKEN         Cloudflare Access JWT for protected routes
  CRABBOX_PROVIDER             %s
  CRABBOX_TARGET               linux, macos, or windows
  CRABBOX_WINDOWS_MODE         normal or wsl2
  CRABBOX_DESKTOP              Provision or require desktop/VNC capability
  CRABBOX_BROWSER              Provision or require browser capability
  CRABBOX_CODE                 Provision or require web code capability
  CRABBOX_STATIC_HOST          Static SSH host for provider=ssh
  CRABBOX_OWNER                Usage owner override
  CRABBOX_ORG                  Usage org override
  CRABBOX_CONFIG               Optional config path
  CRABBOX_IDLE_TIMEOUT         Default idle expiry, e.g. 30m
  CRABBOX_TTL                  Maximum lease lifetime, e.g. 90m
  CRABBOX_AWS_REGION           Default eu-west-1
  CRABBOX_AWS_SSH_CIDRS        Comma-separated AWS SSH source CIDRs
  CRABBOX_SSH_FALLBACK_PORTS   Comma-separated SSH fallback ports, or none
  CRABBOX_CAPACITY_MARKET      spot or on-demand
  CRABBOX_CAPACITY_REGIONS     Comma-separated AWS region fallback candidates
  HCLOUD_TOKEN/HETZNER_TOKEN   Direct Hetzner mode
  CRABBOX_MORPH_API_KEY/MORPH_API_KEY
                               Morph Cloud API key for provider=morph
  CRABBOX_MORPH_SNAPSHOT       Morph snapshot ID for provider=morph
  CRABBOX_PROXMOX_API_URL      Proxmox VE API URL, e.g. https://pve.local:8006
  CRABBOX_PARALLELS_SOURCE     Parallels source VM name for provider=parallels
  CRABBOX_PARALLELS_TEMPLATE   Parallels named template alias

Aliases:
  crabbox release <id-or-slug> Alias for stop
  crabbox pool list            Alias for list
  crabbox machine cleanup      Alias for cleanup

Docs:
  docs/commands/README.md`, providerHelpEnvValues())
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitError{Code: 0}
		}
		return exit(2, "%v", err)
	}
	return nil
}

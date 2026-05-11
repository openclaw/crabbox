package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/alecthomas/kong"
)

type crabboxKongCLI struct {
	Version kong.VersionFlag `name:"version" short:"v" help:"Print version."`

	VersionCmd versionKongCmd    `cmd:"" name:"version" help:"Print version."`
	Init       initKongCmd       `cmd:"" passthrough:"" help:"Onboard the current repo for Crabbox."`
	Login      loginKongCmd      `cmd:"" passthrough:"" help:"Open GitHub login, store broker credentials, verify access."`
	Logout     logoutKongCmd     `cmd:"" passthrough:"" help:"Remove the stored broker token."`
	Whoami     whoamiKongCmd     `cmd:"" passthrough:"" help:"Show broker identity."`
	Doctor     doctorKongCmd     `cmd:"" passthrough:"" help:"Check local and broker/provider readiness."`
	Warmup     warmupKongCmd     `cmd:"" passthrough:"" help:"Lease a box and wait until it is ready."`
	Run        runKongCmd        `cmd:"" passthrough:"" help:"Sync the repo, run a remote command, stream output."`
	Job        jobKongCmd        `cmd:"" help:"Run named repo-local Crabbox jobs."`
	Desktop    desktopKongCmd    `cmd:"" help:"Launch apps into a visible desktop session."`
	Media      mediaKongCmd      `cmd:"" help:"Create preview artifacts from recorded desktop videos."`
	Artifacts  artifactsKongCmd  `cmd:"" help:"Collect, transform, and publish QA artifacts."`
	SyncPlan   syncPlanKongCmd   `cmd:"" name:"sync-plan" passthrough:"" help:"Show local sync manifest size hotspots."`
	History    historyKongCmd    `cmd:"" passthrough:"" help:"List recorded remote runs."`
	Logs       logsKongCmd       `cmd:"" passthrough:"" help:"Print recorded run logs."`
	Events     eventsKongCmd     `cmd:"" passthrough:"" help:"Print recorded run events."`
	Attach     attachKongCmd     `cmd:"" passthrough:"" help:"Follow recorded events for an active run."`
	Results    resultsKongCmd    `cmd:"" passthrough:"" help:"Show recorded test result summaries."`
	Cache      cacheKongCmd      `cmd:"" help:"Inspect, purge, or warm remote caches."`
	Status     statusKongCmd     `cmd:"" passthrough:"" help:"Show lease state; add --wait to block until ready."`
	List       listKongCmd       `cmd:"" passthrough:"" help:"List Crabbox machines."`
	Share      shareKongCmd      `cmd:"" passthrough:"" help:"Share a lease with users or the owning org."`
	Unshare    unshareKongCmd    `cmd:"" passthrough:"" help:"Remove lease sharing."`
	Image      imageKongCmd      `cmd:"" help:"Create or promote brokered AWS runner images."`
	Usage      usageKongCmd      `cmd:"" passthrough:"" help:"Show cost and usage estimates by user, org, or fleet."`
	Admin      adminKongCmd      `cmd:"" help:"Lease admin controls for trusted operators."`
	Actions    actionsKongCmd    `cmd:"" help:"Register GitHub Actions runners or dispatch workflows."`
	Ssh        sshKongCmd        `cmd:"" name:"ssh" passthrough:"" help:"Print the SSH command for a lease."`
	Vnc        vncKongCmd        `cmd:"" name:"vnc" passthrough:"" help:"Print or open VNC connection details for a desktop lease."`
	Webvnc     webvncKongCmd     `cmd:"" name:"webvnc" passthrough:"" help:"Bridge a desktop lease into the authenticated web portal."`
	Code       codeKongCmd       `cmd:"" passthrough:"" help:"Bridge a code lease into the authenticated web portal."`
	Egress     egressKongCmd     `cmd:"" passthrough:"" help:"Bridge lease browser/app traffic through this machine."`
	Screenshot screenshotKongCmd `cmd:"" passthrough:"" help:"Capture a PNG from a desktop lease."`
	Inspect    inspectKongCmd    `cmd:"" passthrough:"" help:"Print lease/provider details; add --json for scripts."`
	Stop       stopKongCmd       `cmd:"" passthrough:"" help:"Release a lease or delete a direct-provider machine."`
	Release    releaseKongCmd    `cmd:"" passthrough:"" help:"Alias for stop."`
	Cleanup    cleanupKongCmd    `cmd:"" passthrough:"" help:"Sweep expired direct-provider machines or local provider state."`
	Azure      azureKongCmd      `cmd:"" help:"Azure provider setup and login."`
	Config     configKongCmd     `cmd:"" help:"Show or update user config."`
	Pool       poolKongCmd       `cmd:"" help:"Alias commands for machine pools."`
	Machine    machineKongCmd    `cmd:"" help:"Alias commands for direct-provider machines."`
}

type kongExit struct {
	code int
}

func (a App) runKong(ctx context.Context, args []string) (err error) {
	args = normalizeKongHelpArgs(args)
	var cli crabboxKongCLI
	parser, err := kong.New(&cli,
		kong.Name("crabbox"),
		kong.Description("Crabbox leases remote test boxes, syncs your dirty checkout, runs commands, and cleans up."),
		kong.Vars{"version": version},
		kong.Writers(a.Stdout, a.Stderr),
		kong.Exit(func(code int) {
			panic(kongExit{code: code})
		}),
	)
	if err != nil {
		return err
	}
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		if exit, ok := recovered.(kongExit); ok {
			if exit.code == 0 {
				err = nil
			} else {
				err = ExitError{Code: exit.code}
			}
			return
		}
		panic(recovered)
	}()
	kctx, err := parser.Parse(args)
	if err != nil {
		var parseErr *kong.ParseError
		if errors.As(err, &parseErr) {
			return exit(2, "%v", parseErr)
		}
		return err
	}
	kctx.BindTo(ctx, (*context.Context)(nil))
	return kctx.Run(a)
}

func normalizeKongHelpArgs(args []string) []string {
	if len(args) > 1 && args[0] == "help" {
		next := append([]string{}, args[1:]...)
		next = append(next, "--help")
		return next
	}
	if isKongCommandGroup(args[0]) && (len(args) == 1 || args[1] == "help") {
		return []string{args[0], "--help"}
	}
	return args
}

func isKongCommandGroup(command string) bool {
	switch command {
	case "actions", "admin", "artifacts", "azure", "cache", "config", "desktop", "image", "job", "machine", "media", "pool":
		return true
	default:
		return false
	}
}

type initKongCmd struct {
	Args []string `arg:"" optional:""`
}
type loginKongCmd struct {
	Args []string `arg:"" optional:""`
}
type logoutKongCmd struct {
	Args []string `arg:"" optional:""`
}
type whoamiKongCmd struct {
	Args []string `arg:"" optional:""`
}
type doctorKongCmd struct {
	Args []string `arg:"" optional:""`
}
type warmupKongCmd struct {
	Args []string `arg:"" optional:""`
}
type runKongCmd struct {
	Args []string `arg:"" optional:""`
}
type jobKongCmd struct {
	List jobListKongCmd `cmd:"" passthrough:"" help:"List configured jobs."`
	Run  jobRunKongCmd  `cmd:"" passthrough:"" help:"Run a configured job."`
}
type jobListKongCmd struct {
	Args []string `arg:"" optional:""`
}
type jobRunKongCmd struct {
	Args []string `arg:"" optional:""`
}
type syncPlanKongCmd struct {
	Args []string `arg:"" optional:""`
}
type historyKongCmd struct {
	Args []string `arg:"" optional:""`
}
type logsKongCmd struct {
	Args []string `arg:"" optional:""`
}
type eventsKongCmd struct {
	Args []string `arg:"" optional:""`
}
type attachKongCmd struct {
	Args []string `arg:"" optional:""`
}
type resultsKongCmd struct {
	Args []string `arg:"" optional:""`
}
type statusKongCmd struct {
	Args []string `arg:"" optional:""`
}
type listKongCmd struct {
	Args []string `arg:"" optional:""`
}
type shareKongCmd struct {
	Args []string `arg:"" optional:""`
}
type unshareKongCmd struct {
	Args []string `arg:"" optional:""`
}
type usageKongCmd struct {
	Args []string `arg:"" optional:""`
}
type sshKongCmd struct {
	Args []string `arg:"" optional:""`
}
type vncKongCmd struct {
	Args []string `arg:"" optional:""`
}
type webvncKongCmd struct {
	Args []string `arg:"" optional:""`
}
type codeKongCmd struct {
	Args []string `arg:"" optional:""`
}
type egressKongCmd struct {
	Args []string `arg:"" optional:""`
}
type screenshotKongCmd struct {
	Args []string `arg:"" optional:""`
}
type inspectKongCmd struct {
	Args []string `arg:"" optional:""`
}
type stopKongCmd struct {
	Args []string `arg:"" optional:""`
}
type releaseKongCmd struct {
	Args []string `arg:"" optional:""`
}
type cleanupKongCmd struct {
	Args []string `arg:"" optional:""`
}

type desktopKongCmd struct {
	Launch   desktopLaunchKongCmd   `cmd:"" passthrough:"" help:"Start an app inside a desktop lease."`
	Terminal desktopTerminalKongCmd `cmd:"" passthrough:"" help:"Start a visible terminal inside a desktop lease."`
	Record   desktopRecordKongCmd   `cmd:"" passthrough:"" help:"Record desktop video from a lease."`
	Proof    desktopProofKongCmd    `cmd:"" passthrough:"" help:"Launch a terminal and collect proof artifacts."`
	Doctor   desktopDoctorKongCmd   `cmd:"" passthrough:"" help:"Check desktop session readiness for a lease."`
	Click    desktopClickKongCmd    `cmd:"" passthrough:"" help:"Click inside a desktop lease."`
	Paste    desktopPasteKongCmd    `cmd:"" passthrough:"" help:"Paste text into a desktop lease."`
	Type     desktopTypeKongCmd     `cmd:"" passthrough:"" help:"Type text into a desktop lease."`
	Key      desktopKeyKongCmd      `cmd:"" passthrough:"" help:"Send keys to a desktop lease."`
}
type desktopLaunchKongCmd struct {
	Args []string `arg:"" optional:""`
}
type desktopTerminalKongCmd struct {
	Args []string `arg:"" optional:""`
}
type desktopRecordKongCmd struct {
	Args []string `arg:"" optional:""`
}
type desktopProofKongCmd struct {
	Args []string `arg:"" optional:""`
}
type desktopDoctorKongCmd struct {
	Args []string `arg:"" optional:""`
}
type desktopClickKongCmd struct {
	Args []string `arg:"" optional:""`
}
type desktopPasteKongCmd struct {
	Args []string `arg:"" optional:""`
}
type desktopTypeKongCmd struct {
	Args []string `arg:"" optional:""`
}
type desktopKeyKongCmd struct {
	Args []string `arg:"" optional:""`
}

type mediaKongCmd struct {
	Preview mediaPreviewKongCmd `cmd:"" passthrough:"" help:"Create a trimmed animated GIF preview from a video."`
}
type mediaPreviewKongCmd struct {
	Args []string `arg:"" optional:""`
}

type artifactsKongCmd struct {
	Collect  artifactsCollectKongCmd  `cmd:"" passthrough:"" help:"Collect screenshots, video, logs, status, and metadata into a bundle."`
	Video    artifactsVideoKongCmd    `cmd:"" passthrough:"" help:"Record an MP4 from a desktop lease."`
	Gif      artifactsGifKongCmd      `cmd:"" passthrough:"" help:"Create a trimmed GIF preview from a video."`
	Template artifactsTemplateKongCmd `cmd:"" passthrough:"" help:"Write Mantis/OpenClaw QA summary markdown."`
	Publish  artifactsPublishKongCmd  `cmd:"" passthrough:"" help:"Upload a bundle and optionally comment inline-ready assets on a PR."`
}
type artifactsCollectKongCmd struct {
	Args []string `arg:"" optional:""`
}
type artifactsVideoKongCmd struct {
	Args []string `arg:"" optional:""`
}
type artifactsGifKongCmd struct {
	Args []string `arg:"" optional:""`
}
type artifactsTemplateKongCmd struct {
	Args []string `arg:"" optional:""`
}
type artifactsPublishKongCmd struct {
	Args []string `arg:"" optional:""`
}

type cacheKongCmd struct {
	List  cacheListKongCmd  `cmd:"" passthrough:"" help:"Show remote cache usage."`
	Stats cacheStatsKongCmd `cmd:"" passthrough:"" help:"Show remote cache usage."`
	Purge cachePurgeKongCmd `cmd:"" passthrough:"" help:"Remove selected cache content."`
	Warm  cacheWarmKongCmd  `cmd:"" passthrough:"" help:"Run a command that populates caches."`
}
type cacheListKongCmd struct {
	Args []string `arg:"" optional:""`
}
type cacheStatsKongCmd struct {
	Args []string `arg:"" optional:""`
}
type cachePurgeKongCmd struct {
	Args []string `arg:"" optional:""`
}
type cacheWarmKongCmd struct {
	Args []string `arg:"" optional:""`
}

type imageKongCmd struct {
	Create  imageCreateKongCmd  `cmd:"" passthrough:"" help:"Create an AMI from a brokered AWS lease."`
	Promote imagePromoteKongCmd `cmd:"" passthrough:"" help:"Promote an AMI for brokered AWS runners."`
}
type imageCreateKongCmd struct {
	Args []string `arg:"" optional:""`
}
type imagePromoteKongCmd struct {
	Args []string `arg:"" optional:""`
}

type adminKongCmd struct {
	Leases  adminLeasesKongCmd  `cmd:"" passthrough:"" help:"List coordinator lease records."`
	Release adminReleaseKongCmd `cmd:"" passthrough:"" help:"Mark a lease released."`
	Delete  adminDeleteKongCmd  `cmd:"" passthrough:"" help:"Delete the backing server and mark the lease released."`
}
type adminLeasesKongCmd struct {
	Args []string `arg:"" optional:""`
}
type adminReleaseKongCmd struct {
	Args []string `arg:"" optional:""`
}
type adminDeleteKongCmd struct {
	Args []string `arg:"" optional:""`
}

type actionsKongCmd struct {
	Hydrate  actionsHydrateKongCmd  `cmd:"" passthrough:"" help:"Register a runner, dispatch the hydrate workflow, wait for readiness."`
	Register actionsRegisterKongCmd `cmd:"" passthrough:"" help:"Register an existing Linux lease as a GitHub Actions runner."`
	Dispatch actionsDispatchKongCmd `cmd:"" passthrough:"" help:"Dispatch the configured GitHub Actions workflow."`
}
type actionsHydrateKongCmd struct {
	Args []string `arg:"" optional:""`
}
type actionsRegisterKongCmd struct {
	Args []string `arg:"" optional:""`
}
type actionsDispatchKongCmd struct {
	Args []string `arg:"" optional:""`
}

type configKongCmd struct {
	Path      configPathKongCmd      `cmd:"" help:"Print the user config path."`
	Show      configShowKongCmd      `cmd:"" passthrough:"" help:"Print merged config without secret values."`
	SetBroker configSetBrokerKongCmd `cmd:"" name:"set-broker" passthrough:"" help:"Store broker URL and optional tokens in user config."`
}

type azureKongCmd struct {
	Login azureLoginKongCmd `cmd:"" passthrough:"" help:"Detect subscription from az CLI, validate credentials, store in user config."`
}
type azureLoginKongCmd struct {
	Args []string `arg:"" optional:""`
}
type configPathKongCmd struct{}
type configShowKongCmd struct {
	Args []string `arg:"" optional:""`
}
type configSetBrokerKongCmd struct {
	Args []string `arg:"" optional:""`
}

type poolKongCmd struct {
	List poolListKongCmd `cmd:"" passthrough:"" help:"Alias for list."`
}
type poolListKongCmd struct {
	Args []string `arg:"" optional:""`
}

type machineKongCmd struct {
	Cleanup machineCleanupKongCmd `cmd:"" passthrough:"" help:"Alias for cleanup."`
}
type machineCleanupKongCmd struct {
	Args []string `arg:"" optional:""`
}

type versionKongCmd struct{}

func (c *initKongCmd) Run(ctx context.Context, app App) error     { return app.initProject(ctx, c.Args) }
func (c *loginKongCmd) Run(ctx context.Context, app App) error    { return app.login(ctx, c.Args) }
func (c *logoutKongCmd) Run(ctx context.Context, app App) error   { return app.logout(ctx, c.Args) }
func (c *whoamiKongCmd) Run(ctx context.Context, app App) error   { return app.whoami(ctx, c.Args) }
func (c *doctorKongCmd) Run(ctx context.Context, app App) error   { return app.doctor(ctx, c.Args) }
func (c *warmupKongCmd) Run(ctx context.Context, app App) error   { return app.warmup(ctx, c.Args) }
func (c *runKongCmd) Run(ctx context.Context, app App) error      { return app.runCommand(ctx, c.Args) }
func (c *jobListKongCmd) Run(ctx context.Context, app App) error  { return app.jobList(ctx, c.Args) }
func (c *jobRunKongCmd) Run(ctx context.Context, app App) error   { return app.jobRun(ctx, c.Args) }
func (c *syncPlanKongCmd) Run(ctx context.Context, app App) error { return app.syncPlan(ctx, c.Args) }
func (c *historyKongCmd) Run(ctx context.Context, app App) error  { return app.history(ctx, c.Args) }
func (c *logsKongCmd) Run(ctx context.Context, app App) error     { return app.logs(ctx, c.Args) }
func (c *eventsKongCmd) Run(ctx context.Context, app App) error   { return app.events(ctx, c.Args) }
func (c *attachKongCmd) Run(ctx context.Context, app App) error   { return app.attach(ctx, c.Args) }
func (c *resultsKongCmd) Run(ctx context.Context, app App) error  { return app.results(ctx, c.Args) }
func (c *statusKongCmd) Run(ctx context.Context, app App) error   { return app.status(ctx, c.Args) }
func (c *listKongCmd) Run(ctx context.Context, app App) error     { return app.list(ctx, c.Args) }
func (c *shareKongCmd) Run(ctx context.Context, app App) error    { return app.share(ctx, c.Args) }
func (c *unshareKongCmd) Run(ctx context.Context, app App) error  { return app.unshare(ctx, c.Args) }
func (c *usageKongCmd) Run(ctx context.Context, app App) error    { return app.usage(ctx, c.Args) }
func (c *sshKongCmd) Run(ctx context.Context, app App) error      { return app.ssh(ctx, c.Args) }
func (c *vncKongCmd) Run(ctx context.Context, app App) error      { return app.vnc(ctx, c.Args) }
func (c *webvncKongCmd) Run(ctx context.Context, app App) error   { return app.webvnc(ctx, c.Args) }
func (c *codeKongCmd) Run(ctx context.Context, app App) error     { return app.webCode(ctx, c.Args) }
func (c *egressKongCmd) Run(ctx context.Context, app App) error   { return app.egress(ctx, c.Args) }
func (c *screenshotKongCmd) Run(ctx context.Context, app App) error {
	return app.screenshot(ctx, c.Args)
}
func (c *inspectKongCmd) Run(ctx context.Context, app App) error { return app.inspect(ctx, c.Args) }
func (c *stopKongCmd) Run(ctx context.Context, app App) error    { return app.stop(ctx, c.Args) }
func (c *releaseKongCmd) Run(ctx context.Context, app App) error { return app.stop(ctx, c.Args) }
func (c *cleanupKongCmd) Run(ctx context.Context, app App) error { return app.cleanup(ctx, c.Args) }

func (c *desktopLaunchKongCmd) Run(ctx context.Context, app App) error {
	return app.desktopLaunch(ctx, c.Args)
}
func (c *desktopTerminalKongCmd) Run(ctx context.Context, app App) error {
	return app.desktopTerminal(ctx, c.Args)
}
func (c *desktopRecordKongCmd) Run(ctx context.Context, app App) error {
	return app.desktopRecord(ctx, c.Args)
}
func (c *desktopProofKongCmd) Run(ctx context.Context, app App) error {
	return app.desktopProof(ctx, c.Args)
}
func (c *desktopDoctorKongCmd) Run(ctx context.Context, app App) error {
	return app.desktopDoctor(ctx, c.Args)
}
func (c *desktopClickKongCmd) Run(ctx context.Context, app App) error {
	return app.desktopClick(ctx, c.Args)
}
func (c *desktopPasteKongCmd) Run(ctx context.Context, app App) error {
	return app.desktopPaste(ctx, c.Args)
}
func (c *desktopTypeKongCmd) Run(ctx context.Context, app App) error {
	return app.desktopType(ctx, c.Args)
}
func (c *desktopKeyKongCmd) Run(ctx context.Context, app App) error {
	return app.desktopKey(ctx, c.Args)
}

func (c *mediaPreviewKongCmd) Run(ctx context.Context, app App) error {
	return app.mediaPreview(ctx, c.Args)
}

func (c *artifactsCollectKongCmd) Run(ctx context.Context, app App) error {
	return app.artifactsCollect(ctx, stripKongCommandPath(c.Args, "artifacts", "collect"))
}
func (c *artifactsVideoKongCmd) Run(ctx context.Context, app App) error {
	return app.artifactsVideo(ctx, stripKongCommandPath(c.Args, "artifacts", "video"))
}
func (c *artifactsGifKongCmd) Run(ctx context.Context, app App) error {
	return app.artifactsGif(ctx, stripKongCommandPath(c.Args, "artifacts", "gif"))
}
func (c *artifactsTemplateKongCmd) Run(ctx context.Context, app App) error {
	return app.artifactsTemplate(ctx, stripKongCommandPath(c.Args, "artifacts", "template"))
}
func (c *artifactsPublishKongCmd) Run(ctx context.Context, app App) error {
	return app.artifactsPublish(ctx, stripKongCommandPath(c.Args, "artifacts", "publish"))
}

func (c *cacheListKongCmd) Run(ctx context.Context, app App) error {
	return app.cacheStats(ctx, c.Args)
}
func (c *cacheStatsKongCmd) Run(ctx context.Context, app App) error {
	return app.cacheStats(ctx, c.Args)
}
func (c *cachePurgeKongCmd) Run(ctx context.Context, app App) error {
	return app.cachePurge(ctx, c.Args)
}
func (c *cacheWarmKongCmd) Run(ctx context.Context, app App) error { return app.cacheWarm(ctx, c.Args) }

func (c *imageCreateKongCmd) Run(ctx context.Context, app App) error {
	return app.imageCreate(ctx, c.Args)
}
func (c *imagePromoteKongCmd) Run(ctx context.Context, app App) error {
	return app.imagePromote(ctx, c.Args)
}

func (c *adminLeasesKongCmd) Run(ctx context.Context, app App) error {
	return app.adminLeases(ctx, c.Args)
}
func (c *adminReleaseKongCmd) Run(ctx context.Context, app App) error {
	return app.adminRelease(ctx, c.Args)
}
func (c *adminDeleteKongCmd) Run(ctx context.Context, app App) error {
	return app.adminDelete(ctx, c.Args)
}

func (c *actionsHydrateKongCmd) Run(ctx context.Context, app App) error {
	return app.actionsHydrate(ctx, c.Args)
}
func (c *actionsRegisterKongCmd) Run(ctx context.Context, app App) error {
	return app.actionsRegister(ctx, c.Args)
}
func (c *actionsDispatchKongCmd) Run(ctx context.Context, app App) error {
	return app.actionsDispatch(ctx, c.Args)
}

func (c *configPathKongCmd) Run(ctx context.Context, app App) error {
	path := userConfigPath()
	if path == "" {
		return exit(2, "user config directory is unavailable")
	}
	fmt.Fprintln(app.Stdout, path)
	return nil
}
func (c *configShowKongCmd) Run(app App) error {
	return app.configShow(c.Args)
}
func (c *configSetBrokerKongCmd) Run(app App) error {
	return app.configSetBroker(c.Args)
}

func (c *azureLoginKongCmd) Run(ctx context.Context, app App) error {
	return app.azureLogin(ctx, c.Args)
}

func (c *poolListKongCmd) Run(ctx context.Context, app App) error {
	return app.list(ctx, c.Args)
}
func (c *machineCleanupKongCmd) Run(ctx context.Context, app App) error {
	return app.cleanup(ctx, c.Args)
}

func (c *versionKongCmd) Run(app App) error {
	fmt.Fprintln(app.Stdout, version)
	return nil
}

func stripKongCommandPath(args []string, path ...string) []string {
	out := append([]string{}, args...)
	for _, part := range path {
		if len(out) == 0 || out[0] != part {
			return out
		}
		out = out[1:]
	}
	return out
}

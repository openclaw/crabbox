package cli

//go:generate go run ../../scripts/configgen -source config_warpbuild.go -output config_warpbuild_generated.go -type WarpBuildConfig -provider warpbuild

// WarpBuildConfig keeps the native CLI and its credential in trusted inputs.
type WarpBuildConfig struct {
	Token       string `env:"CRABBOX_WARPBUILD_TOKEN" envAlias:"WB_TOKEN" sources:"env"`
	CLIPath     string `config:"cliPath" env:"CRABBOX_WARPBUILD_CLI" flag:"warpbuild-cli" sources:"user,env,flag" help:"Path to the WarpBuild wb CLI" default:"wb" fileIgnoreEmpty:"true" fileStorage:"value"`
	RunnerLabel string `config:"runnerLabel" env:"CRABBOX_WARPBUILD_RUNNER_LABEL" flag:"warpbuild-runner-label" sources:"user,repo,env,flag" help:"WarpBuild macOS sandbox runner label" default:"warp-macos-26-arm64-6x" fileIgnoreEmpty:"true" fileStorage:"value"`
	Workdir     string `config:"workdir" env:"CRABBOX_WARPBUILD_WORKDIR" flag:"warpbuild-workdir" sources:"user,repo,env,flag" help:"Absolute workspace directory inside the WarpBuild sandbox" default:"/Users/runner/crabbox" fileIgnoreEmpty:"true" fileStorage:"value"`
}

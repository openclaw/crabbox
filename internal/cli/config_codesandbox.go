package cli

//go:generate go run ../../scripts/configgen -source config_codesandbox.go -output config_codesandbox_generated.go -type CodeSandboxConfig -provider codesandbox

// CodeSandboxConfig describes mechanical bindings for the delegated provider.
// BridgeCommand and SDKPackage retain trusted-file-only admission. The API key
// stays in runtime environment authentication, never in config or on argv.
type CodeSandboxConfig struct {
	TemplateID               string `config:"templateId" env:"CRABBOX_CODESANDBOX_TEMPLATE_ID" flag:"codesandbox-template-id" sources:"user,repo,env,flag" help:"CodeSandbox template ID used by later lifecycle operations"`
	Workdir                  string `config:"workdir" env:"CRABBOX_CODESANDBOX_WORKDIR" flag:"codesandbox-workdir" sources:"user,repo,env,flag" help:"Absolute working directory inside the sandbox; must be under /project/workspace" default:"/project/workspace"`
	VMTier                   string `config:"vmTier" env:"CRABBOX_CODESANDBOX_VM_TIER" flag:"codesandbox-vm-tier" sources:"user,repo,env,flag" help:"CodeSandbox VM tier for later create operations (empty = workspace default)"`
	Privacy                  string `config:"privacy" env:"CRABBOX_CODESANDBOX_PRIVACY" flag:"codesandbox-privacy" sources:"user,repo,env,flag" help:"CodeSandbox sandbox privacy for later create operations" default:"private"`
	HibernationTimeoutSecs   int    `config:"hibernationTimeoutSecs" env:"CRABBOX_CODESANDBOX_HIBERNATION_TIMEOUT_SECS" flag:"codesandbox-hibernation-timeout-secs" sources:"user,repo,env,flag" help:"CodeSandbox hibernation timeout in seconds (0 = service default)" nonnegative:"true"`
	AutomaticWakeupHTTP      bool   `config:"automaticWakeupHTTP" env:"CRABBOX_CODESANDBOX_AUTOMATIC_WAKEUP_HTTP" flag:"codesandbox-automatic-wakeup-http" sources:"user,repo,env,flag" help:"allow automatic wakeup on HTTP requests" default:"true"`
	AutomaticWakeupWebSocket bool   `config:"automaticWakeupWebSocket" env:"CRABBOX_CODESANDBOX_AUTOMATIC_WAKEUP_WEBSOCKET" flag:"codesandbox-automatic-wakeup-websocket" sources:"user,repo,env,flag" help:"allow automatic wakeup on WebSocket connections"`
	BridgeCommand            string `config:"bridgeCommand" env:"CRABBOX_CODESANDBOX_BRIDGE_COMMAND" flag:"codesandbox-bridge-command" sources:"user,env,flag" help:"local Node-compatible command used for the CodeSandbox SDK bridge" default:"node"`
	SDKPackage               string `config:"sdkPackage" env:"CRABBOX_CODESANDBOX_SDK_PACKAGE" flag:"codesandbox-sdk-package" sources:"user,env,flag" help:"Node package spec imported by the CodeSandbox SDK bridge" default:"@codesandbox/sdk@2.4.2"`
	DoctorListLimit          int    `config:"doctorListLimit" env:"CRABBOX_CODESANDBOX_DOCTOR_LIST_LIMIT" flag:"codesandbox-doctor-list-limit" sources:"user,repo,env,flag" help:"maximum sandboxes read by non-mutating doctor readiness" default:"1" nonnegative:"true"`
	OperationTimeoutSecs     int    `config:"operationTimeoutSecs" env:"CRABBOX_CODESANDBOX_OPERATION_TIMEOUT_SECS" flag:"codesandbox-operation-timeout-secs" sources:"user,repo,env,flag" help:"SDK bridge operation timeout in seconds" default:"30" nonnegative:"true"`
}

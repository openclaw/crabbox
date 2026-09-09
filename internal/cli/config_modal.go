package cli

//go:generate go run ../../scripts/configgen -source config_modal.go -output config_modal_generated.go -type ModalConfig -provider modal

// ModalConfig describes mechanical bindings for the delegated Modal provider.
// Secrets contains names; native secret lookup and authentication stay in Modal.
type ModalConfig struct {
	App         string   `config:"app" env:"CRABBOX_MODAL_APP" flag:"modal-app" sources:"user,repo,env,flag" help:"Modal app name for Crabbox sandboxes" default:"crabbox" fileIgnoreEmpty:"true"`
	Image       string   `config:"image" env:"CRABBOX_MODAL_IMAGE" flag:"modal-image" sources:"user,repo,env,flag" help:"Modal sandbox image, as a registry reference" default:"python:3.13-slim" fileIgnoreEmpty:"true"`
	Workdir     string   `config:"workdir" env:"CRABBOX_MODAL_WORKDIR" flag:"modal-workdir" sources:"user,repo,env,flag" help:"Absolute working directory inside the Modal sandbox" default:"/workspace/crabbox" fileIgnoreEmpty:"true"`
	Python      string   `config:"python" env:"CRABBOX_MODAL_PYTHON" flag:"modal-python" sources:"user,repo,env,flag" help:"Python binary used to run the local Modal client" default:"python3" fileIgnoreEmpty:"true"`
	Environment string   `config:"environment" env:"CRABBOX_MODAL_ENVIRONMENT" flag:"modal-environment" sources:"user,env,flag" help:"Modal environment for the sandbox and named Secrets" fileIgnoreEmpty:"true"`
	Secrets     []string `config:"secrets" env:"CRABBOX_MODAL_SECRETS" flag:"modal-secret" sources:"user,env,flag" help:"named Modal Secret to inject into the sandbox; repeatable or comma-separated" fileList:"raw" envList:"presence" flagList:"replace-append"`
}

package cli

import "strings"

//go:generate go run ../../scripts/configgen -source config_aws_lambda_microvm.go -output config_aws_lambda_microvm_generated.go -type AWSLambdaMicroVMConfig -provider awsLambdaMicroVM

// AWSLambdaMicroVMConfig owns the seven provider bindings; AWSRegion stays flat.
type AWSLambdaMicroVMConfig struct {
	Image             string   `config:"image" env:"CRABBOX_AWS_LAMBDA_MICROVM_IMAGE" flag:"aws-lambda-microvm-image" sources:"user,repo,env,flag" help:"Lambda MicroVM image ARN" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	ImageVersion      string   `config:"imageVersion" env:"CRABBOX_AWS_LAMBDA_MICROVM_IMAGE_VERSION" flag:"aws-lambda-microvm-image-version" sources:"user,repo,env,flag" help:"Lambda MicroVM image version (default latest active)" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	ExecutionRoleARN  string   `config:"executionRoleArn" env:"CRABBOX_AWS_LAMBDA_MICROVM_EXECUTION_ROLE_ARN" flag:"aws-lambda-microvm-execution-role-arn" sources:"user,repo,env,flag" help:"optional IAM execution role ARN" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Workdir           string   `config:"workdir" env:"CRABBOX_AWS_LAMBDA_MICROVM_WORKDIR" flag:"aws-lambda-microvm-workdir" sources:"user,repo,env,flag" help:"absolute runner workdir" default:"/workspace/crabbox" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	IngressConnectors []string `config:"ingressConnectors" env:"CRABBOX_AWS_LAMBDA_MICROVM_INGRESS_CONNECTORS" flag:"aws-lambda-microvm-ingress-connectors" sources:"user,repo,env,flag" help:"comma-separated ingress connector ARNs" fileList:"raw" envList:"csv" flagList:"scalar-empty-nil"`
	EgressConnectors  []string `config:"egressConnectors" env:"CRABBOX_AWS_LAMBDA_MICROVM_EGRESS_CONNECTORS" flag:"aws-lambda-microvm-egress-connectors" sources:"user,repo,env,flag" help:"comma-separated egress connector ARNs" fileList:"raw" envList:"csv" flagList:"scalar-empty-nil"`
	ForgetMissing     bool     `config:"forgetMissing" env:"CRABBOX_AWS_LAMBDA_MICROVM_FORGET_MISSING" flag:"aws-lambda-microvm-forget-missing" sources:"user,repo,env,flag" help:"remove local claim when the MicroVM is already missing"`
}

// NormalizeAppliedFlags must not be used for raw file or environment overlays.
func (cfg *AWSLambdaMicroVMConfig) NormalizeAppliedFlags(applied AWSLambdaMicroVMConfigApplied) {
	if applied.Image {
		cfg.Image = strings.TrimSpace(cfg.Image)
	}
	if applied.ImageVersion {
		cfg.ImageVersion = strings.TrimSpace(cfg.ImageVersion)
	}
	if applied.ExecutionRoleARN {
		cfg.ExecutionRoleARN = strings.TrimSpace(cfg.ExecutionRoleARN)
	}
	if applied.Workdir {
		cfg.Workdir = strings.TrimSpace(cfg.Workdir)
	}
}

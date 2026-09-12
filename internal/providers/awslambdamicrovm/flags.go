package awslambdamicrovm

import core "github.com/openclaw/crabbox/internal/cli"

import (
	"flag"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
)

type flagValues struct {
	Region *string
	Config core.AWSLambdaMicroVMConfigFlagValues
}

func registerFlags(fs *flag.FlagSet, defaults Config) any {
	return flagValues{
		Region: fs.String("aws-lambda-microvm-region", defaults.AWSRegion, "AWS Region for Lambda MicroVMs"),
		Config: core.RegisterAWSLambdaMicroVMConfigFlags(fs, defaults.AWSLambdaMicroVM),
	}
}

func applyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "aws-lambda-microvm-region") {
		cfg.AWSRegion = strings.TrimSpace(*v.Region)
		core.RecordProviderFlagInputs(cfg, true, "aws", providerName)
	}
	applied, err := v.Config.Apply(&cfg.AWSLambdaMicroVM, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	cfg.AWSLambdaMicroVM.NormalizeAppliedFlags(applied)
	if err != nil {
		return err
	}
	return validateConfig(*cfg)
}

var awsRegionPattern = regexp.MustCompile(`^[a-z]{2}(?:-gov)?-[a-z]+-\d+$`)

func validateConfig(cfg Config) error {
	region := strings.TrimSpace(cfg.AWSRegion)
	if !awsRegionPattern.MatchString(region) {
		return exit(2, "invalid AWS Lambda MicroVM region %q", cfg.AWSRegion)
	}
	image := strings.TrimSpace(cfg.AWSLambdaMicroVM.Image)
	if image == "" {
		return exit(3, "AWS Lambda MicroVM image is required; set CRABBOX_AWS_LAMBDA_MICROVM_IMAGE or --aws-lambda-microvm-image")
	}
	parsed, err := arn.Parse(image)
	if err != nil || parsed.Service != "lambda" || parsed.Region != region || !strings.HasPrefix(parsed.Resource, "microvm-image:") {
		return exit(2, "invalid AWS Lambda MicroVM image ARN for region %s", region)
	}
	if role := strings.TrimSpace(cfg.AWSLambdaMicroVM.ExecutionRoleARN); role != "" {
		parsedRole, err := arn.Parse(role)
		if err != nil || parsedRole.Service != "iam" || !strings.HasPrefix(parsedRole.Resource, "role/") {
			return exit(2, "invalid AWS Lambda MicroVM execution role ARN")
		}
	}
	workdir := strings.TrimSpace(cfg.AWSLambdaMicroVM.Workdir)
	if workdir == "" || !strings.HasPrefix(workdir, "/") || path.Clean(workdir) != workdir || strings.Contains(workdir, "\x00") {
		return exit(2, "aws-lambda-microvm workdir must be a clean absolute path below /")
	}
	if awsLambdaMicroVMBroadWorkdir(workdir) {
		return exit(2, "aws-lambda-microvm workdir %q is too broad; choose a dedicated subdirectory", workdir)
	}
	if cfg.IdleTimeout > 0 && cfg.IdleTimeout < time.Minute {
		return exit(2, "aws-lambda-microvm idle timeout must be at least 60s")
	}
	for kind, connectors := range map[string][]string{
		"ingress": cfg.AWSLambdaMicroVM.IngressConnectors,
		"egress":  cfg.AWSLambdaMicroVM.EgressConnectors,
	} {
		for _, connector := range connectors {
			if err := validateConnectorARN(connector, region); err != nil {
				return exit(2, "invalid AWS Lambda MicroVM %s connector: %v", kind, err)
			}
		}
	}
	return nil
}

func awsLambdaMicroVMBroadWorkdir(workdir string) bool {
	switch path.Clean(workdir) {
	case "/", "/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib64", "/media", "/mnt", "/opt", "/proc", "/root", "/run", "/sbin", "/srv", "/sys", "/tmp", "/usr", "/var", "/work", "/workspace":
		return true
	default:
		return false
	}
}

func validateConnectorARN(value, region string) error {
	parsed, err := arn.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Service != "lambda" || parsed.Region != region || !strings.HasPrefix(parsed.Resource, "network-connector:") {
		return fmt.Errorf("expected Lambda network-connector ARN in region %s", region)
	}
	return nil
}

package cli

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAWSLambdaMicroVMConfigYAMLAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	var file fileConfig
	input := []byte(`awsLambdaMicroVM:
  image: arn:aws:lambda:eu-west-1:123456789012:microvm-image:runner
  imageVersion: "2"
  executionRoleArn: arn:aws:iam::123456789012:role/MicrovmRuntime
  workdir: /work/app
  ingressConnectors: [arn:aws:lambda:eu-west-1:aws:network-connector:aws-network-connector:ALL_INGRESS]
  egressConnectors: [arn:aws:lambda:eu-west-1:aws:network-connector:aws-network-connector:INTERNET_EGRESS]
  forgetMissing: true
`)
	if err := yaml.Unmarshal(input, &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.AWSLambdaMicroVM.ImageVersion != "2" || cfg.AWSLambdaMicroVM.Workdir != "/work/app" || !cfg.AWSLambdaMicroVM.ForgetMissing {
		t.Fatalf("YAML config=%#v", cfg.AWSLambdaMicroVM)
	}
	if len(cfg.AWSLambdaMicroVM.IngressConnectors) != 1 || len(cfg.AWSLambdaMicroVM.EgressConnectors) != 1 {
		t.Fatalf("YAML connectors=%#v", cfg.AWSLambdaMicroVM)
	}

	t.Setenv("CRABBOX_AWS_LAMBDA_MICROVM_IMAGE_VERSION", "3")
	t.Setenv("CRABBOX_AWS_LAMBDA_MICROVM_WORKDIR", "/work/env")
	t.Setenv("CRABBOX_AWS_LAMBDA_MICROVM_INGRESS_CONNECTORS", "ingress-a, ingress-b")
	t.Setenv("CRABBOX_AWS_LAMBDA_MICROVM_EGRESS_CONNECTORS", "egress-a")
	t.Setenv("CRABBOX_AWS_LAMBDA_MICROVM_FORGET_MISSING", "false")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AWSLambdaMicroVM.ImageVersion != "3" || cfg.AWSLambdaMicroVM.Workdir != "/work/env" || cfg.AWSLambdaMicroVM.ForgetMissing {
		t.Fatalf("env config=%#v", cfg.AWSLambdaMicroVM)
	}
	if !reflect.DeepEqual(cfg.AWSLambdaMicroVM.IngressConnectors, []string{"ingress-a", "ingress-b"}) || !reflect.DeepEqual(cfg.AWSLambdaMicroVM.EgressConnectors, []string{"egress-a"}) {
		t.Fatalf("env connectors=%#v", cfg.AWSLambdaMicroVM)
	}
}

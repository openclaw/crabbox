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

func TestAWSLambdaMicroVMConnectorOverlays(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      []string
	}{
		{"empty", "", []string{"inherited"}},
		{"whitespace", " \t ", nil},
		{"commas", " , , ", []string{}},
		{"items", " a , b , a ", []string{"a", "b", "a"}},
		{"literal-none", "none", []string{"none"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.AWSLambdaMicroVM.IngressConnectors = []string{"inherited"}
			cfg.AWSLambdaMicroVM.EgressConnectors = []string{"inherited"}
			t.Setenv("CRABBOX_AWS_LAMBDA_MICROVM_INGRESS_CONNECTORS", tc.raw)
			t.Setenv("CRABBOX_AWS_LAMBDA_MICROVM_EGRESS_CONNECTORS", tc.raw)
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			for _, got := range [][]string{cfg.AWSLambdaMicroVM.IngressConnectors, cfg.AWSLambdaMicroVM.EgressConnectors} {
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("list=%#v, want %#v", got, tc.want)
				}
			}
		})
	}
	for _, raw := range []string{"null", "[]", "[' a ', b, ' a ']"} {
		t.Run("file-"+raw, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.AWSLambdaMicroVM.IngressConnectors = []string{"inherited"}
			cfg.AWSLambdaMicroVM.EgressConnectors = []string{"inherited"}
			var file fileConfig
			if err := yaml.Unmarshal([]byte("awsLambdaMicroVM:\n  ingressConnectors: "+raw+"\n  egressConnectors: "+raw+"\n  forgetMissing: false\n"), &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfig(&cfg, file); err != nil {
				t.Fatal(err)
			}
			want := []string{" a ", "b", " a "}
			if raw == "null" {
				want = []string{"inherited"}
			} else if raw == "[]" {
				want = nil
			}
			for _, got := range [][]string{cfg.AWSLambdaMicroVM.IngressConnectors, cfg.AWSLambdaMicroVM.EgressConnectors} {
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("file list=%#v, want %#v", got, want)
				}
			}
			encoded, err := yaml.Marshal(file)
			if err != nil {
				t.Fatal(err)
			}
			var roundtrip fileConfig
			if err := yaml.Unmarshal(encoded, &roundtrip); err != nil || !reflect.DeepEqual(file.AWSLambdaMicroVM, roundtrip.AWSLambdaMicroVM) {
				t.Fatalf("raw file roundtrip changed: %s (%v)", encoded, err)
			}
			if raw != "null" && raw != "[]" {
				(*file.AWSLambdaMicroVM.IngressConnectors)[0] = "changed"
				(*file.AWSLambdaMicroVM.EgressConnectors)[0] = "changed"
				if cfg.AWSLambdaMicroVM.IngressConnectors[0] != " a " || cfg.AWSLambdaMicroVM.EgressConnectors[0] != " a " {
					t.Fatal("runtime lists alias the file lists")
				}
			}
		})
	}
}

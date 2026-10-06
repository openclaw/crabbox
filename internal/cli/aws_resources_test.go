package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAWSResourceRequirementsFilterBeforeProvisioning(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cpu, memory int
		metadata    string
		want        string
	}{
		{"undersized", 4, 8192, "<vCpuInfo><defaultVCpus>2</defaultVCpus></vCpuInfo><memoryInfo><sizeInMiB>2048</sizeInMiB></memoryInfo>", "no AWS launch candidates satisfy"},
		{"missing cpu", 4, 0, "<memoryInfo><sizeInMiB>32768</sizeInMiB></memoryInfo>", "metadata is unavailable or invalid"},
		{"zero memory", 0, 8192, "<memoryInfo><sizeInMiB>0</sizeInMiB></memoryInfo>", "metadata is unavailable or invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}
				if r.Form.Get("Action") != "DescribeInstanceTypes" {
					t.Errorf("unexpected effect: %s", r.Form.Get("Action"))
				}
				writeEC2XML(w, "<DescribeInstanceTypesResponse><instanceTypeSet><item><instanceType>custom.metal</instanceType>"+tc.metadata+"</item></instanceTypeSet></DescribeInstanceTypesResponse>")
			}))
			defer server.Close()
			cfg := baseConfig()
			cfg.Provider, cfg.TargetOS, cfg.ServerType, cfg.ServerTypeExplicit = "aws", targetLinux, "custom.metal", true
			cfg.Capacity.MinVCPUs, cfg.Capacity.MinMemoryMiB = tc.cpu, tc.memory
			_, _, err := testAWSClient(server.URL).createServerWithFallbackInRegion(context.Background(), cfg, "", "cbx_123", "test", false, nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v", err)
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestAWSResourceRequirementsPreserveCandidateOrderAndIndependentDimensions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cpu, memory int
		dimension   string
	}{
		{"CPU only", 8, 0, "<vCpuInfo><defaultVCpus>8</defaultVCpus></vCpuInfo>"},
		{"memory only", 0, 24576, "<memoryInfo><sizeInMiB>24576</sizeInMiB></memoryInfo>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Provider, cfg.TargetOS, cfg.Class = "aws", targetLinux, "standard"
			cfg.ServerType = "m7a.4xlarge"
			cfg.Capacity.MinVCPUs, cfg.Capacity.MinMemoryMiB = tc.cpu, tc.memory
			candidates := AWSLaunchCandidates(cfg)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}
				if r.Form.Get("Action") != "DescribeInstanceTypes" {
					t.Fatal(r.Form)
				}
				// Reverse provider response order, and
				// describe the final fallback below the requested dimension.
				var items strings.Builder
				for i := len(candidates) - 1; i >= 0; i-- {
					dimension := tc.dimension
					if i == len(candidates)-1 {
						dimension = "<vCpuInfo><defaultVCpus>1</defaultVCpus></vCpuInfo><memoryInfo><sizeInMiB>1024</sizeInMiB></memoryInfo>"
					}
					fmt.Fprintf(&items, "<item><instanceType>%s</instanceType>%s</item>", candidates[i], dimension)
				}
				writeEC2XML(w, "<DescribeInstanceTypesResponse><instanceTypeSet>"+items.String()+"</instanceTypeSet></DescribeInstanceTypesResponse>")
			}))
			defer server.Close()
			got, err := testAWSClient(server.URL).resourceQualifiedLaunchCandidates(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, candidates[:len(candidates)-1]) {
				t.Fatalf("candidates=%v", got)
			}
		})
	}
}

func TestAWSResourceRequirementsUnconstrainedNeedsNoMetadata(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider, cfg.TargetOS, cfg.Class = "aws", targetLinux, "standard"
	got, err := (&AWSClient{}).resourceQualifiedLaunchCandidates(context.Background(), cfg)
	if err != nil || !reflect.DeepEqual(got, AWSLaunchCandidates(cfg)) {
		t.Fatalf("got=%v err=%v", got, err)
	}
}

func TestCoordinatorResourceRequirementsRouteAndWire(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		for _, constrained := range []bool{false, true} {
			t.Run(fmt.Sprintf("fixed=%t constrained=%t", fixed, constrained), func(t *testing.T) {
				var requests int
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					want := "/v1/leases"
					method := http.MethodPost
					if fixed {
						want += "/cbx_0123456789ab"
						method = http.MethodPut
					}
					if constrained {
						want += "/resource-constrained"
					}
					if r.Method != method || r.URL.Path != want {
						t.Errorf("request=%s %s", r.Method, r.URL.Path)
					}
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					capacity, _ := body["capacity"].(map[string]any)
					if constrained {
						if capacity["minVCPUs"] != float64(8) {
							t.Errorf("capacity=%v", capacity)
						}
					} else if _, exists := capacity["minVCPUs"]; exists {
						t.Errorf("zero serialized: %v", capacity)
					}
					if _, exists := capacity["minMemoryMiB"]; exists {
						t.Errorf("absent memory serialized: %v", capacity)
					}
					http.NotFound(w, r)
				}))
				defer server.Close()
				cfg := baseConfig()
				cfg.Provider, cfg.TargetOS = "aws", targetLinux
				if constrained {
					cfg.Capacity.MinVCPUs = 8
				}
				client := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
				_, err := client.createLease(context.Background(), cfg, "ssh-key", true, "cbx_0123456789ab", "test", "attempt", fixed)
				if err == nil || requests != 1 {
					t.Fatalf("err=%v requests=%d", err, requests)
				}
			})
		}
	}
}

func TestCapacityMinimumConfigFlagsAndReuse(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Provider, cfg.TargetOS = "aws", targetLinux
	cpu, memory := 8, 24576
	if err := applyFileConfig(&cfg, fileConfig{Capacity: &fileCapacityConfig{MinVCPUs: &cpu, MinMemoryMiB: &memory}}); err != nil {
		t.Fatal(err)
	}
	fs := newFlagSet("run", io.Discard)
	flags := registerLeaseCreateFlags(fs, cfg)
	if err := parseFlags(fs, []string{"--min-vcpus=0"}); err != nil {
		t.Fatal(err)
	}
	if err := applyLeaseCreateFlagsForLeaseMode(&cfg, fs, flags, "", false); err != nil {
		t.Fatal(err)
	}
	if cfg.Capacity.MinVCPUs != 0 || cfg.Capacity.MinMemoryMiB != memory {
		t.Fatalf("capacity=%+v", cfg.Capacity)
	}
	if err := applyLeaseCreateFlagsForLeaseMode(&cfg, fs, flags, "cbx_0123456789ab", false); err == nil || !strings.Contains(err.Error(), "reuse") {
		t.Fatalf("reuse err=%v", err)
	}
	for _, value := range []int{-1, 2147483647} {
		cfg.Capacity.MinVCPUs = value
		err := validateCapacityMinimumValues(cfg.Capacity)
		if (err != nil) != (value < 0) {
			t.Fatalf("value=%d err=%v", value, err)
		}
	}
}

func TestAWSResourceRequirementsLaunchOnlyQualifiedCandidate(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider, cfg.TargetOS, cfg.Class, cfg.ServerType = "aws", targetLinux, "c7a.4xlarge", "c7a.8xlarge"
	cfg.AWSAMI, cfg.AWSSGID, cfg.AWSRootGB = "ami-test", "sg-test", 400
	cfg.Capacity.Market, cfg.Capacity.MinVCPUs = "on-demand", 8
	cfg.SSHPort, cfg.SSHFallbackPorts = "22", nil
	cfg.ProviderKey = "test-key"
	candidates := AWSLaunchCandidates(cfg)
	want := candidates[1]
	publicKey := testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 1))
	fingerprints, err := awsImportedPublicKeyFingerprints(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		action := r.Form.Get("Action")
		actions = append(actions, action)
		switch action {
		case "DescribeInstanceTypes":
			writeEC2XML(w, "<DescribeInstanceTypesResponse><instanceTypeSet><item><instanceType>"+want+"</instanceType><vCpuInfo><defaultVCpus>8</defaultVCpus></vCpuInfo></item></instanceTypeSet></DescribeInstanceTypesResponse>")
		case "DescribeKeyPairs":
			writeEC2XML(w, "<DescribeKeyPairsResponse><keySet><item><keyName>test-key</keyName><keyFingerprint>"+fingerprints[0]+"</keyFingerprint></item></keySet></DescribeKeyPairsResponse>")
		case "DescribeSecurityGroups":
			writeEC2XML(w, "<DescribeSecurityGroupsResponse><securityGroupInfo><item><groupId>sg-test</groupId></item></securityGroupInfo></DescribeSecurityGroupsResponse>")
		case "AuthorizeSecurityGroupIngress":
			writeEC2XML(w, "<AuthorizeSecurityGroupIngressResponse/>")
		case "RunInstances":
			if r.Form.Get("InstanceType") != want {
				t.Errorf("launched=%s want=%s", r.Form.Get("InstanceType"), want)
			}
			writeEC2XML(w, "<RunInstancesResponse><instancesSet><item><instanceId>i-created</instanceId><instanceState><name>pending</name></instanceState><instanceType>"+want+"</instanceType></item></instancesSet></RunInstancesResponse>")
		default:
			t.Errorf("unexpected AWS action %s", action)
			writeEC2Error(w, "Unexpected", action, http.StatusBadRequest)
		}
	}))
	defer server.Close()
	created, resolved, err := testAWSClient(server.URL).createServerWithFallbackInRegion(context.Background(), cfg, publicKey, "cbx_0123456789ab", "test", true, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if created.CloudID != "i-created" || resolved.ServerType != want || actions[0] != "DescribeInstanceTypes" {
		t.Fatalf("server=%+v type=%s actions=%v", created, resolved.ServerType, actions)
	}
}

func TestCapacityMinimumYAMLRejectsNonIntegersAndBounds(t *testing.T) {
	for _, value := range []string{"-1", "1.5", "1.0", "2147483648", "null", "'8'"} {
		for _, name := range []string{"minVCPUs", "minMemoryMiB"} {
			var config fileConfig
			if err := yaml.Unmarshal([]byte("capacity:\n  "+name+": "+value+"\n"), &config); err == nil {
				t.Errorf("accepted %s=%s", name, value)
			}
		}
	}
}

func TestFixedAWSResourceRequirementsBindOnlyPositiveDimensions(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	req := FixedAWSCreateIntentRequest{AccountID: "123456789012", RequestedSlug: "test", SSHPublicKey: "ssh-key"}
	baseline, err := FixedAWSCreateIntentFingerprint(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	for _, capacity := range []CapacityConfig{{MinVCPUs: 8}, {MinMemoryMiB: 24576}, {MinVCPUs: 8, MinMemoryMiB: 24576}} {
		candidate := cfg
		candidate.Capacity.MinVCPUs, candidate.Capacity.MinMemoryMiB = capacity.MinVCPUs, capacity.MinMemoryMiB
		got, err := FixedAWSCreateIntentFingerprint(candidate, req)
		if err != nil || got == baseline {
			t.Fatalf("capacity=%+v hash=%s err=%v", capacity, got, err)
		}
		intent, err := fixedAWSCreateIntentForConfig(candidate, req)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(intent)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"minVCPUs":0`) || strings.Contains(string(encoded), `"minMemoryMiB":0`) {
			t.Fatalf("zero serialized: %s", encoded)
		}
	}
}

func TestCapacityMinimumUnsupportedTargetsAndCompositeCommands(t *testing.T) {
	for _, provider := range []string{"aws", "azure"} {
		for _, target := range []string{targetLinux, targetWindows, targetMacOS} {
			cfg := baseConfig()
			cfg.Provider, cfg.TargetOS, cfg.Capacity.MinVCPUs = provider, target, 4
			err := validateResourceRequirements(cfg)
			if (err == nil) != (provider == "aws" && target == targetLinux) {
				t.Errorf("provider=%s target=%s err=%v", provider, target, err)
			}
		}
	}
	for _, command := range []string{"prewarm", "checkpoint fork"} {
		cfg := baseConfig()
		cfg.Provider, cfg.TargetOS, cfg.Capacity.MinVCPUs = "aws", targetLinux, 4
		fs := newFlagSet(command, io.Discard)
		flags := registerLeaseCreateFlags(fs, cfg)
		if err := applyLeaseCreateFlagsForLeaseMode(&cfg, fs, flags, "", false); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Errorf("command=%s err=%v", command, err)
		}
	}
}

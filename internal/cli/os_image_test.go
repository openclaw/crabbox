package cli

import "testing"

func TestNormalizeOSImage(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":             "ubuntu:26.04",
		"ubuntu:26.04": "ubuntu:26.04",
		"ubuntu-26.04": "ubuntu:26.04",
		"ubuntu2604":   "ubuntu:26.04",
		"ubuntu:24.04": "ubuntu:24.04",
		"ubuntu-24.04": "ubuntu:24.04",
		"ubuntu2404":   "ubuntu:24.04",
	}
	for input, want := range cases {
		input, want := input, want
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeOSImage(input)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("normalizeOSImage(%q)=%q want %q", input, got, want)
			}
		})
	}
}

func TestAWSLinuxAMIQueryForOS(t *testing.T) {
	t.Parallel()
	name, label, err := awsLinuxAMIQueryForOS("ubuntu:26.04", ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if name != "ubuntu/images/hvm-ssd-gp3/ubuntu-resolute-26.04-amd64-server-*" || label != "Ubuntu 26.04" {
		t.Fatalf("query name=%q label=%q", name, label)
	}
	name, label, err = awsLinuxAMIQueryForOS("ubuntu:24.04", ArchitectureARM64)
	if err != nil {
		t.Fatal(err)
	}
	if name != "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-arm64-server-*" || label != "Ubuntu 24.04" {
		t.Fatalf("arm query name=%q label=%q", name, label)
	}
}

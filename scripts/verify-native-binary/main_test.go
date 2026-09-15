package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestVerifyNativeHost(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("release executable formats")
	}
	file, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := verify(file, runtime.GOOS, runtime.GOARCH)
	if err != nil || identity.Architecture != runtime.GOARCH || identity.Bits != 64 {
		t.Fatalf("host executable: %+v, %v", identity, err)
	}
	other := "arm64"
	if runtime.GOARCH == other {
		other = "amd64"
	}
	if _, err := verify(file, runtime.GOOS, other); err == nil {
		t.Fatal("host executable satisfied another architecture")
	}
}

func TestVerifyNativeCrossBuilds(t *testing.T) {
	directory := os.Getenv("CRABBOX_TEST_NATIVE_BINARY_DIR")
	if directory == "" {
		t.Skip("requires ordinary six-target compiler outputs")
	}
	for _, platform := range []string{"darwin", "linux", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			t.Run(platform+"/"+arch, func(t *testing.T) {
				identity, err := verify(filepath.Join(directory, platform+"_"+arch), platform, arch)
				if err != nil || identity.Architecture != arch || identity.Bits != 64 {
					t.Fatalf("cross-built executable: %+v, %v", identity, err)
				}
			})
		}
	}
}

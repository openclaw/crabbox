// runtime-artifacts prepares and verifies finalized local runtime packs without
// executing their contents. The caller owns source provenance and publication.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/openclaw/crabbox/internal/remoteruntime"
	"github.com/openclaw/crabbox/internal/runtimeartifact"
)

const usage = `Usage: runtime-artifacts <prepare|verify> --directory DIR --controller FILE
       runtime-artifacts extract --archive FILE --directory NEW_DIR --os OS --arch ARCH --runtime-pack none|unsigned|final

  prepare  Print a manifest for finalized linux-amd64 and linux-arm64 files.
  verify   Verify DIR/manifest.json and both runtime targets; print JSON identities.
  extract  Stage an exact release archive in a new private directory; print JSON evidence.

Inputs are read-only. No configuration, network, build, execution, or prompts.
The caller writes prepare output atomically only after controller signing and
all runtime bytes are final. Hash pairing does not establish source provenance.

Exit codes: 0 success, 1 verification or I/O failure, 2 invalid arguments.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	for i, arg := range args {
		if arg == "-h" || arg == "--help" || i == 0 && arg == "help" {
			if _, err := io.WriteString(stdout, usage); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			return 0
		}
	}
	if len(args) == 0 || args[0] != "prepare" && args[0] != "verify" && args[0] != "extract" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	directory := fs.String("directory", "", "runtime pack directory")
	if args[0] == "extract" {
		archive := fs.String("archive", "", "release archive")
		platform := fs.String("os", "", "controller operating system")
		arch := fs.String("arch", "", "controller architecture")
		mode := fs.String("runtime-pack", "", "none, unsigned, or final")
		if err := fs.Parse(args[1:]); err != nil {
			fmt.Fprintf(stderr, "%v; use --help\n", err)
			return 2
		}
		if fs.NArg() != 0 || *directory == "" || *archive == "" || !validArchiveTarget(*platform, *arch) || !validArchiveMode(*mode) {
			fmt.Fprintln(stderr, "extract requires --archive, a new --directory, --os, --arch, and --runtime-pack; use --help")
			return 2
		}
		report, err := extractArchive(ctx, *archive, *directory, *platform, *arch, *mode)
		if err == nil {
			err = json.NewEncoder(stdout).Encode(report)
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	controller := fs.String("controller", "", "final controller executable")
	if err := fs.Parse(args[1:]); err != nil {
		fmt.Fprintf(stderr, "%v; use --help\n", err)
		return 2
	}
	if fs.NArg() != 0 || *directory == "" || *controller == "" {
		fmt.Fprintln(stderr, "--directory and --controller are required; no positional arguments; use --help")
		return 2
	}
	inputs := []runtimeartifact.ArtifactInput{
		{Target: runtimeartifact.Target{OS: "linux", Arch: "amd64"}, Path: "linux-amd64"},
		{Target: runtimeartifact.Target{OS: "linux", Arch: "arm64"}, Path: "linux-arm64"},
	}
	var err error
	if args[0] == "prepare" {
		var data []byte
		data, err = runtimeartifact.MarshalLocal(ctx, *directory, *controller, inputs, remoteruntime.Protocol)
		if err == nil {
			var written int
			written, err = stdout.Write(data)
			if err == nil && written != len(data) {
				err = io.ErrShortWrite
			}
		}
	} else {
		err = verify(ctx, *directory, *controller, inputs, stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func verify(ctx context.Context, directory, controller string, inputs []runtimeartifact.ArtifactInput, stdout io.Writer) error {
	set, err := runtimeartifact.OpenLocalSet(ctx, filepath.Join(directory, "manifest.json"), controller, remoteruntime.Protocol)
	if err != nil {
		return err
	}
	type artifactIdentity struct {
		OS     string `json:"os"`
		Arch   string `json:"arch"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	result := struct {
		Protocol  string             `json:"protocolVersion"`
		Artifacts []artifactIdentity `json:"artifacts"`
	}{Protocol: remoteruntime.Protocol}
	for _, input := range inputs {
		artifact, err := set.Open(ctx, input.Target)
		if err != nil {
			return err
		}
		identity := artifact.Identity()
		if err := artifact.Close(); err != nil {
			return err
		}
		result.Artifacts = append(result.Artifacts, artifactIdentity{identity.Target.OS, identity.Target.Arch, identity.Size, identity.SHA256})
	}
	return json.NewEncoder(stdout).Encode(result)
}

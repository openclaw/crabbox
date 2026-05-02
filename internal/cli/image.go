package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Hetzner live snapshots silently lose unflushed pagecache writes; the
// optional fsfreeze adds a block-level barrier on xfs/btrfs.
const hetznerPreSnapshotFlush = `set -e
sync; sync
sudo -n fsfreeze -f / >/dev/null 2>&1 && sudo -n fsfreeze -u / >/dev/null 2>&1 || true
`

func (a App) image(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return exit(2, "usage: crabbox image <create|promote> [flags]")
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprintln(a.Stdout, `Usage:
  crabbox image create --id <cbx_id> --name <ami-name> [--wait]
  crabbox image promote <ami-id>`)
		return nil
	case "create":
		return a.imageCreate(ctx, args[1:])
	case "promote":
		return a.imagePromote(ctx, args[1:])
	default:
		return exit(2, "unknown image command %q", args[0])
	}
}

func (a App) imageCreate(ctx context.Context, args []string) error {
	fs := newFlagSet("image create", a.Stderr)
	id := fs.String("id", "", "AWS lease id to image")
	name := fs.String("name", "", "AMI name")
	wait := fs.Bool("wait", false, "wait until the AMI is available")
	waitTimeout := fs.Duration("wait-timeout", 45*time.Minute, "maximum wait duration")
	noReboot := fs.Bool("no-reboot", true, "avoid rebooting the source instance while creating the AMI")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" || *name == "" {
		return exit(2, "usage: crabbox image create --id <cbx_id> --name <ami-name> [--wait]")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	coord, ok, err := newCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	if !ok {
		return exit(2, "image create requires a coordinator")
	}
	if err := flushHetznerLeaseBeforeSnapshot(ctx, coord, cfg, *id, a.Stderr); err != nil {
		fmt.Fprintf(a.Stderr, "warn: pre-snapshot flush failed: %v (proceeding anyway)\n", err)
	}
	image, err := coord.CreateImage(ctx, *id, *name, *noReboot)
	if err != nil {
		return err
	}
	if *wait {
		image, err = waitForImage(ctx, coord, image.ID, *waitTimeout, a.Stderr)
		if err != nil {
			return err
		}
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(image)
	}
	fmt.Fprintf(a.Stdout, "image=%s name=%s state=%s region=%s\n", image.ID, image.Name, image.State, blank(image.Region, "-"))
	return nil
}

func (a App) imagePromote(ctx context.Context, args []string) error {
	fs := newFlagSet("image promote", a.Stderr)
	tag := fs.String("tag", "", "promotion tag (default \"latest\")")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox image promote <image-id> [--tag <name>]")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	coord, ok, err := newCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	if !ok {
		return exit(2, "image promote requires a coordinator")
	}
	image, err := coord.PromoteImage(ctx, fs.Arg(0), *tag)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(image)
	}
	fmt.Fprintf(a.Stdout, "promoted image=%s tag=%s name=%s state=%s region=%s\n", image.ID, blank(image.Tag, "latest"), image.Name, image.State, blank(image.Region, "-"))
	return nil
}

func flushHetznerLeaseBeforeSnapshot(ctx context.Context, coord *CoordinatorClient, cfg Config, leaseID string, stderr io.Writer) error {
	lease, err := coord.GetLease(ctx, leaseID)
	if err != nil {
		return err
	}
	if lease.Provider != "hetzner" {
		return nil
	}
	if lease.Host == "" {
		return fmt.Errorf("lease %s has no host", leaseID)
	}
	target := sshTargetForLease(cfg, lease.Host, lease.SSHUser, lease.SSHPort)
	target.FallbackPorts = lease.SSHFallbackPorts
	useStoredTestboxKey(&target, lease.ID)
	fmt.Fprintf(stderr, "flushing lease %s before snapshot\n", lease.ID)
	return runSSHQuiet(ctx, target, hetznerPreSnapshotFlush)
}

func waitForImage(ctx context.Context, coord *CoordinatorClient, imageID string, timeout time.Duration, stderr io.Writer) (CoordinatorImage, error) {
	deadline := time.Now().Add(timeout)
	var last CoordinatorImage
	for {
		image, err := coord.Image(ctx, imageID)
		if err != nil {
			return CoordinatorImage{}, err
		}
		last = image
		if image.State == "available" {
			return image, nil
		}
		if image.State == "failed" {
			return CoordinatorImage{}, exit(5, "image %s failed", imageID)
		}
		if time.Now().After(deadline) {
			return CoordinatorImage{}, exit(5, "timed out waiting for image %s; last state=%s", imageID, last.State)
		}
		_, _ = fmt.Fprintf(stderr, "waiting image=%s state=%s\n", imageID, blank(image.State, "pending"))
		select {
		case <-ctx.Done():
			return CoordinatorImage{}, ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

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
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	image, err := coord.CreateImage(ctx, *id, *name, *noReboot)
	if err != nil {
		return err
	}
	if *wait {
		image, err = waitForImage(ctx, coord, image.ID, image.Region, *waitTimeout, a.Stderr)
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
	target := fs.String("target", "", "AWS image target: linux, macos, or windows")
	region := fs.String("region", "", "AWS region containing the AMI")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox image promote <ami-id> [--target linux|macos|windows] [--region <aws-region>]")
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	image, err := coord.PromoteImage(ctx, fs.Arg(0), *target, *region)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(image)
	}
	fmt.Fprintf(a.Stdout, "promoted image=%s name=%s state=%s region=%s\n", image.ID, image.Name, image.State, blank(image.Region, "-"))
	return nil
}

func (a App) imageDelete(ctx context.Context, args []string) error {
	fs := newFlagSet("image delete", a.Stderr)
	region := fs.String("region", "", "AWS region containing the AMI")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox image delete <ami-id> [--region <region>]")
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	if err := coord.DeleteImage(ctx, fs.Arg(0), *region); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "deleted image=%s region=%s\n", fs.Arg(0), blank(*region, "-"))
	return nil
}

func waitForImage(ctx context.Context, coord *CoordinatorClient, imageID, region string, timeout time.Duration, stderr io.Writer) (CoordinatorImage, error) {
	deadline := time.Now().Add(timeout)
	var last CoordinatorImage
	for {
		image, err := coord.Image(ctx, imageID, region)
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

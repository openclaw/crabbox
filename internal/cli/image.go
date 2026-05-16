package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

func (a App) imageCreate(ctx context.Context, args []string) error {
	fs := newFlagSet("image create", a.Stderr)
	id := fs.String("id", "", "lease id to image")
	name := fs.String("name", "", "provider image name")
	wait := fs.Bool("wait", false, "wait until the provider image is available")
	waitTimeout := fs.Duration("wait-timeout", 45*time.Minute, "maximum wait duration")
	noReboot := fs.Bool("no-reboot", true, "avoid rebooting the source AWS instance while creating the AMI")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" || *name == "" {
		return exit(2, "usage: crabbox image create --id <cbx_id> --name <image-name> [--wait]")
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	image, err := coord.CreateImage(ctx, *id, *name, *noReboot, checkpointStrategyImage)
	if err != nil {
		return err
	}
	if *wait {
		image, err = waitForImage(ctx, coord, image.ID, imageRefFromCoordinatorImage(image), *waitTimeout, a.Stderr)
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
	serverType := fs.String("type", "", "AWS instance type the AMI boots on, for example mac1.metal")
	serverTypeAlias := fs.String("server-type", "", "alias for --type")
	architecture := fs.String("architecture", "", "AWS AMI architecture, for example x86_64_mac or arm64_mac")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox image promote <ami-id> [--target linux|macos|windows] [--region <aws-region>] [--type <instance-type>] [--architecture <arch>]")
	}
	if *serverType == "" {
		*serverType = *serverTypeAlias
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	image, err := coord.PromoteImage(ctx, fs.Arg(0), CoordinatorImageRef{
		Provider:     "aws",
		Region:       *region,
		Target:       *target,
		ServerType:   *serverType,
		Architecture: *architecture,
	})
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
	provider := fs.String("provider", "aws", "image provider: aws, azure, or gcp")
	region := fs.String("region", "", "region, location, or zone containing the image")
	project := fs.String("project", "", "GCP project containing the image")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox image delete <image-id> [--provider aws|azure|gcp] [--region <region>] [--project <project>]")
	}
	normalizedProvider := normalizeProviderName(*provider)
	if normalizedProvider != "aws" && normalizedProvider != "azure" && normalizedProvider != "gcp" {
		return exit(2, "unsupported image provider %q; use aws, azure, or gcp", *provider)
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	ref := CoordinatorImageRef{Provider: normalizedProvider, Region: *region, Project: *project}
	if err := coord.DeleteImage(ctx, fs.Arg(0), ref); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "deleted image=%s provider=%s region=%s project=%s\n", fs.Arg(0), normalizedProvider, blank(*region, "-"), blank(*project, "-"))
	return nil
}

func waitForImage(ctx context.Context, coord *CoordinatorClient, imageID string, ref CoordinatorImageRef, timeout time.Duration, stderr io.Writer) (CoordinatorImage, error) {
	deadline := time.Now().Add(timeout)
	var last CoordinatorImage
	for {
		image, err := coord.Image(ctx, imageID, ref)
		if err != nil {
			return CoordinatorImage{}, err
		}
		last = image
		state := strings.ToLower(image.State)
		if state == "available" || state == "ready" || state == "succeeded" || state == "completed" {
			return image, nil
		}
		if state == "failed" || state == "invalid" {
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

func imageRefFromCoordinatorImage(image CoordinatorImage) CoordinatorImageRef {
	return CoordinatorImageRef{
		Provider:     image.Provider,
		Region:       image.Region,
		Project:      image.Project,
		Kind:         image.Kind,
		Target:       image.Target,
		ServerType:   image.ServerType,
		Architecture: image.Architecture,
	}
}

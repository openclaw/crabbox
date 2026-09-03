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
	provider := fs.String("provider", "aws", "image provider: aws or azure")
	target := fs.String("target", "", "image target: linux, macos, or windows")
	osImage := fs.String("os", "", "portable Linux OS selector for promoted Linux AMIs")
	region := fs.String("region", "", "provider region or Azure location containing the image")
	serverType := fs.String("type", "", "AWS instance type or Azure VM size the image boots on")
	serverTypeAlias := fs.String("server-type", "", "alias for --type")
	architecture := fs.String("architecture", "", "AWS AMI architecture, for example x86_64_mac or arm64_mac")
	osVersion := fs.String("os-version", "", "OS version present in the image")
	var sdkVersions stringListFlag
	var runtimeVersions stringListFlag
	var variantSDKVersions stringListFlag
	var variantRuntimeVersions stringListFlag
	fs.Var(&sdkVersions, "sdk", "SDK present in name=version form; repeatable")
	fs.Var(&runtimeVersions, "runtime", "runtime present in name=version form; repeatable")
	fs.Var(&variantSDKVersions, "variant-sdk", "SDK that explicitly activates a catalog-only image; repeatable")
	fs.Var(&variantRuntimeVersions, "variant-runtime", "runtime that explicitly activates a catalog-only image; repeatable")
	browser := fs.Bool("browser", false, "image includes a browser")
	webView2 := fs.Bool("webview2", false, "image includes Microsoft WebView2")
	desktop := fs.Bool("desktop", false, "image includes desktop support")
	catalogOnly := fs.Bool("catalog-only", false, "publish an AWS capability variant without changing the default image")
	fastSnapshotRestore := fs.Bool("fast-snapshot-restore", false, "enable AWS Fast Snapshot Restore for the promoted AMI snapshots")
	var fastSnapshotRestoreAZs stringListFlag
	fs.Var(&fastSnapshotRestoreAZs, "fsr-az", "availability zone for Fast Snapshot Restore; repeatable")
	expectedCurrentImage := fs.String("expected-current-image", "", "expected current image id, none, or capture")
	expectedCurrentRevision := fs.String("expected-current-revision", "", "expected current default revision when an image is present")
	retireExpectedCatalog := fs.Bool("retire-expected-catalog", false, "retire the exact expected AWS catalog revision during rollback")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox image promote <image-id> [--provider aws|azure] [--target linux|macos|windows] [--os ubuntu:26.04|ubuntu:24.04] [--region <region>] [--type <instance-type>] [--architecture <arch>] [--os-version <version>] [--sdk <name=version>] [--runtime <name=version>] [--variant-sdk <name=version>] [--variant-runtime <name=version>] [--browser] [--webview2] [--desktop] [--catalog-only] [--fast-snapshot-restore --fsr-az <az>]")
	}
	normalizedProvider := normalizeProviderName(*provider)
	if normalizedProvider != "aws" && normalizedProvider != "azure" {
		return exit(2, "unsupported image promotion provider %q; use aws or azure", *provider)
	}
	if normalizedProvider == "azure" && *catalogOnly {
		return exit(2, "--catalog-only is AWS-only")
	}
	if normalizedProvider == "azure" && (*fastSnapshotRestore || len(fastSnapshotRestoreAZs) > 0) {
		return exit(2, "Fast Snapshot Restore is AWS-only")
	}
	if normalizedProvider == "azure" && flagWasSet(fs, "expected-current-image") {
		return exit(2, "compare-and-swap image promotion is AWS-only")
	}
	if *catalogOnly && (flagWasSet(fs, "expected-current-image") || *retireExpectedCatalog) {
		return exit(2, "--catalog-only cannot be combined with transactional image promotion")
	}
	if *retireExpectedCatalog && !flagWasSet(fs, "expected-current-image") {
		return exit(2, "--retire-expected-catalog requires --expected-current-image and --expected-current-revision")
	}
	if *serverType == "" {
		*serverType = *serverTypeAlias
	}
	if err := validateImageVersion(strings.TrimSpace(*osVersion), "os-version"); err != nil {
		return err
	}
	sdks, err := parseImageVersions(sdkVersions, "sdk")
	if err != nil {
		return err
	}
	runtimes, err := parseImageVersions(runtimeVersions, "runtime")
	if err != nil {
		return err
	}
	variantSDKs, err := parseImageVersions(variantSDKVersions, "variant-sdk")
	if err != nil {
		return err
	}
	variantRuntimes, err := parseImageVersions(variantRuntimeVersions, "variant-runtime")
	if err != nil {
		return err
	}
	variantSelectors := imageVariantSelectors{SDKs: variantSDKs, Runtimes: variantRuntimes}
	if *catalogOnly && imageVariantSelectorsEmpty(variantSelectors) {
		return exit(2, "--catalog-only requires at least one --variant-sdk or --variant-runtime declaration")
	}
	if !*catalogOnly && !imageVariantSelectorsEmpty(variantSelectors) {
		return exit(2, "--variant-sdk and --variant-runtime require --catalog-only")
	}
	sdks, err = mergeImageVersions(sdks, variantSDKs, "sdk", "variant-sdk")
	if err != nil {
		return err
	}
	runtimes, err = mergeImageVersions(runtimes, variantRuntimes, "runtime", "variant-runtime")
	if err != nil {
		return err
	}
	if normalizedProvider == "azure" &&
		(strings.TrimSpace(*osVersion) != "" ||
			len(sdks) > 0 ||
			len(runtimes) > 0 ||
			*browser ||
			*webView2 ||
			*desktop) {
		return exit(2, "image capability declarations are AWS-only")
	}
	if flagWasSet(fs, "os") {
		normalized, err := normalizeOSImage(*osImage)
		if err != nil {
			return err
		}
		*osImage = normalized
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	ref := CoordinatorImageRef{
		Provider:               normalizedProvider,
		Region:                 *region,
		Target:                 *target,
		OSImage:                *osImage,
		ServerType:             *serverType,
		Architecture:           *architecture,
		CatalogOnly:            *catalogOnly,
		FastSnapshotRestore:    *fastSnapshotRestore,
		FastSnapshotRestoreAZs: fastSnapshotRestoreAZs,
		Capabilities: imageCapabilities{
			OSVersion: strings.TrimSpace(*osVersion),
			SDKs:      sdks,
			Runtimes:  runtimes,
			Browser:   *browser,
			WebView2:  *webView2,
			Desktop:   *desktop,
		},
		VariantSelectors: variantSelectors,
	}
	var image CoordinatorImage
	if flagWasSet(fs, "expected-current-image") {
		expected, err := imageExpectedCurrent(*expectedCurrentImage, *expectedCurrentRevision)
		if err != nil {
			return err
		}
		if *retireExpectedCatalog && expected.State != "present" {
			return exit(2, "--retire-expected-catalog requires an exact expected current image and revision")
		}
		var result CoordinatorImagePromotionResult
		if fs.Arg(0) == "none" {
			if expected.State != "present" {
				return exit(2, "clearing a default requires an expected current image and revision")
			}
			result, err = coord.PromoteImageCAS(ctx, expected.ImageID, expected, true, *retireExpectedCatalog, ref)
		} else {
			result, err = coord.PromoteImageCAS(ctx, fs.Arg(0), expected, false, *retireExpectedCatalog, ref)
		}
		if err != nil {
			return err
		}
		if *jsonOut {
			return json.NewEncoder(a.Stdout).Encode(result)
		}
		if fs.Arg(0) == "none" {
			fmt.Fprintln(a.Stdout, "promoted image=none")
			return nil
		}
		if result.Image == nil {
			return fmt.Errorf("coordinator did not return the promoted image")
		}
		image = *result.Image
	} else {
		if fs.Arg(0) == "none" {
			return exit(2, "promoting image none requires --expected-current-image and --expected-current-revision")
		}
		if flagWasSet(fs, "expected-current-revision") {
			return exit(2, "--expected-current-revision requires --expected-current-image")
		}
		image, err = coord.PromoteImage(ctx, fs.Arg(0), ref)
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(image)
	}
	fmt.Fprintf(a.Stdout, "promoted image=%s name=%s state=%s region=%s", image.ID, image.Name, image.State, blank(image.Region, "-"))
	if image.CatalogOnly {
		fmt.Fprint(a.Stdout, " catalogOnly=true")
	}
	fmt.Fprintln(a.Stdout)
	return nil
}

func imageExpectedCurrent(imageID, revision string) (CoordinatorImageDefaultState, error) {
	imageID = strings.TrimSpace(imageID)
	revision = strings.TrimSpace(revision)
	if imageID == "none" {
		if revision != "" {
			return CoordinatorImageDefaultState{}, exit(2, "--expected-current-revision is invalid when --expected-current-image=none")
		}
		return CoordinatorImageDefaultState{State: "absent"}, nil
	}
	if imageID == "capture" {
		if revision != "" {
			return CoordinatorImageDefaultState{}, exit(2, "--expected-current-revision is invalid when --expected-current-image=capture")
		}
		return CoordinatorImageDefaultState{State: "capture"}, nil
	}
	if imageID == "" {
		return CoordinatorImageDefaultState{}, exit(2, "--expected-current-image must be an image id, none, or capture")
	}
	if revision == "" {
		return CoordinatorImageDefaultState{}, exit(2, "--expected-current-revision is required when the expected current image is present")
	}
	return CoordinatorImageDefaultState{State: "present", ImageID: imageID, Revision: revision}, nil
}

func (a App) imageFSRStatus(ctx context.Context, args []string) error {
	fs := newFlagSet("image fsr-status", a.Stderr)
	provider := fs.String("provider", "aws", "image provider: aws")
	region := fs.String("region", "", "AWS region containing the AMI or snapshot")
	var fastSnapshotRestoreAZs stringListFlag
	fs.Var(&fastSnapshotRestoreAZs, "fsr-az", "availability zone for Fast Snapshot Restore status; repeatable")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox image fsr-status <ami-id|snapshot-id> [--region <aws-region>] [--fsr-az <az>]")
	}
	normalizedProvider := normalizeProviderName(*provider)
	if normalizedProvider != "aws" {
		return exit(2, "unsupported image provider %q; Fast Snapshot Restore is AWS-only", *provider)
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	image, err := coord.FastSnapshotRestoreStatus(ctx, fs.Arg(0), CoordinatorImageRef{
		Provider:               normalizedProvider,
		Region:                 *region,
		FastSnapshotRestoreAZs: fastSnapshotRestoreAZs,
	})
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(image)
	}
	fmt.Fprintf(a.Stdout, "image=%s state=%s region=%s fsr=%d\n", image.ID, blank(image.State, "-"), blank(image.Region, "-"), len(image.FastSnapshotRestores))
	if len(image.FastSnapshotRestores) == 0 {
		fmt.Fprintln(a.Stdout, "fsr none")
		return nil
	}
	for _, status := range image.FastSnapshotRestores {
		fmt.Fprintf(a.Stdout, "fsr snapshot=%s az=%s state=%s reason=%s\n", status.SnapshotID, status.AvailabilityZone, blank(status.State, "-"), blank(status.StateTransitionReason, "-"))
	}
	return nil
}

func (a App) imageDelete(ctx context.Context, args []string) error {
	fs := newFlagSet("image delete", a.Stderr)
	provider := fs.String("provider", "aws", "image provider: aws, azure, gcp, or hetzner")
	region := fs.String("region", "", "region, location, or zone containing the image")
	project := fs.String("project", "", "GCP project containing the image")
	catalogOnly := fs.Bool("catalog-only", false, "unpublish AWS catalog-only variants without deleting the AMI")
	retirePromotions := fs.Bool("retire-promotions", false, "retire AWS or Azure image catalog/default roles without deleting provider resources")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox image delete <image-id> [--provider aws|azure|gcp|hetzner] [--region <region>] [--project <project>] [--catalog-only|--retire-promotions]")
	}
	normalizedProvider := normalizeProviderName(*provider)
	if normalizedProvider != "aws" && normalizedProvider != "azure" && normalizedProvider != "gcp" && normalizedProvider != "hetzner" {
		return exit(2, "unsupported image provider %q; use aws, azure, gcp, or hetzner", *provider)
	}
	if *catalogOnly && normalizedProvider != "aws" {
		return exit(2, "--catalog-only is AWS-only")
	}
	if *retirePromotions && normalizedProvider != "aws" && normalizedProvider != "azure" {
		return exit(2, "--retire-promotions supports AWS and Azure only")
	}
	if *retirePromotions && *catalogOnly {
		return exit(2, "--catalog-only and --retire-promotions are mutually exclusive")
	}
	if normalizedProvider == "hetzner" {
		if strings.TrimSpace(*project) != "" {
			return exit(2, "--project is not supported for Hetzner image deletion")
		}
		store, err := defaultCheckpointStore()
		if err != nil {
			return err
		}
		lifecycle, ok := nativeCheckpointLifecycleProvider(Config{Provider: "hetzner"}, Server{})
		if !ok {
			return exit(2, "Hetzner snapshot lifecycle provider is unavailable")
		}
		record, err := deleteHetznerCheckpointImage(ctx, store, lifecycle, fs.Arg(0), strings.TrimSpace(*region))
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "deleted image=%s provider=hetzner region=%s project=- checkpoint=%s\n", fs.Arg(0), blank(record.Native.Region, "-"), record.ID)
		return nil
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	ref := CoordinatorImageRef{Provider: normalizedProvider, Region: *region, Project: *project}
	if *retirePromotions {
		retired, err := coord.RetirePromotedImage(ctx, fs.Arg(0), ref)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "retired promoted image=%s provider=%s roles=%d\n", fs.Arg(0), normalizedProvider, retired.Retired)
		return nil
	}
	if *catalogOnly {
		retired, err := coord.RetireCatalogImage(ctx, fs.Arg(0), ref)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "retired catalog-only image=%s provider=aws variants=%d\n", fs.Arg(0), retired.Retired)
		return nil
	}
	if err := coord.DeleteImage(ctx, fs.Arg(0), ref); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "deleted image=%s provider=%s region=%s project=%s\n", fs.Arg(0), normalizedProvider, blank(*region, "-"), blank(*project, "-"))
	return nil
}

func deleteHetznerCheckpointImage(ctx context.Context, store checkpointStore, lifecycle NativeCheckpointLifecycleProvider, imageID, region string) (checkpointRecord, error) {
	records, err := store.List()
	if err != nil {
		return checkpointRecord{}, err
	}
	matches := make([]checkpointRecord, 0, 1)
	for _, record := range records {
		if record.Kind == checkpointKindHetzner && record.Native.ImageID == imageID {
			matches = append(matches, record)
		}
	}
	if len(matches) == 0 {
		return checkpointRecord{}, exit(2, "refusing to delete Hetzner snapshot %s without an exact local hetzner-snapshot checkpoint record", imageID)
	}
	if len(matches) > 1 {
		return checkpointRecord{}, exit(2, "refusing to delete Hetzner snapshot %s because %d local checkpoint records claim it", imageID, len(matches))
	}
	record := matches[0]
	if region != "" && region != record.Native.Region {
		return checkpointRecord{}, exit(2, "Hetzner snapshot %s location mismatch: recorded=%s requested=%s", imageID, blank(record.Native.Region, "unknown"), region)
	}
	if unresolvedCheckpoint(record) {
		return checkpointRecord{}, exit(2, "checkpoint %s is unresolved; reconcile its capture before deletion", record.ID)
	}
	if err := lifecycle.DeleteNativeCheckpoint(ctx, nativeCheckpointResourceRequest(record)); err != nil {
		return checkpointRecord{}, err
	}
	if err := store.Delete(record.ID); err != nil {
		return checkpointRecord{}, err
	}
	return record, nil
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

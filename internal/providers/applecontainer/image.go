package applecontainer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type retainedImageContainerError struct {
	name, leaseID string
	cause         error
}

func (e *retainedImageContainerError) Error() string {
	return fmt.Sprintf("%v; retained Apple container target=%s lease=%s and SSH key for manual native inspection/cleanup; bootstrap was not confirmed", e.cause, e.name, e.leaseID)
}
func (e *retainedImageContainerError) Unwrap() error { return e.cause }

type imageContainerObservation struct {
	container     inspectContainer
	configuration any
}

func (b *backend) imageControl(ctx context.Context, cfg core.Config, args []string, timeout time.Duration) (core.LocalCommandResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return core.LocalCommandResult{}, err
	}
	const limit = 1024 * 1024
	result, err := b.rt.Exec.Run(ctx, core.LocalCommandRequest{
		Name: cfg.AppleContainer.CLIPath, Args: args,
		MaxCapturedOutputBytes: limit, CancelGracePeriod: time.Second,
	})
	if ctx.Err() != nil {
		return core.LocalCommandResult{}, ctx.Err()
	}
	if err != nil || result.ExitCode != 0 || len(result.Stdout) >= limit || len(result.Stderr) >= limit {
		return core.LocalCommandResult{}, exit(5, "Apple Container %s image verification command failed (native response withheld)", args[0])
	}
	return result, nil
}

func (b *backend) inspectImageContainer(ctx context.Context, cfg core.Config, name string) (imageContainerObservation, error) {
	result, err := b.imageControl(ctx, cfg, []string{"inspect", name}, 30*time.Second)
	if err != nil {
		return imageContainerObservation{}, err
	}
	var entries []json.RawMessage
	if json.Unmarshal([]byte(result.Stdout), &entries) != nil || len(entries) != 1 {
		return imageContainerObservation{}, exit(5, "Apple Container inspect must return exactly one created container")
	}
	var c inspectContainer
	var raw struct {
		Configuration json.RawMessage `json:"configuration"`
	}
	if json.Unmarshal(entries[0], &c) != nil || json.Unmarshal(entries[0], &raw) != nil || c.id() != name {
		return imageContainerObservation{}, exit(5, "Apple Container inspect did not return the exact created target")
	}
	var configuration any
	decoder := json.NewDecoder(bytes.NewReader(raw.Configuration))
	decoder.UseNumber()
	if decoder.Decode(&configuration) != nil || configuration == nil {
		return imageContainerObservation{}, exit(5, "Apple Container inspect omitted its created configuration")
	}
	return imageContainerObservation{c, configuration}, nil
}

func ownedStoppedImageContainer(c inspectContainer, image, leaseID, slug string) bool {
	if c.status() != "created" && c.status() != "stopped" {
		return false
	}
	if c.image() != image || !validImageDigest(c.Configuration.Image.Descriptor.Digest) {
		return false
	}
	for key, value := range map[string]string{"crabbox": "true", "provider": providerName, "lease": leaseID, "slug": slug, "image": image} {
		if c.labels()[key] != value {
			return false
		}
		if top, ok := c.Labels[key]; ok && top != value {
			return false
		}
	}
	return true
}

func validImageDigest(digest string) bool {
	if len(digest) != 71 || !strings.HasPrefix(digest, "sha256:") {
		return false
	}
	for _, ch := range digest[7:] {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

// Apple's image resolver can trust a registry-returned digest different from
// the requested one. Verify the created configuration before start, which uses
// that stored image and does not resolve the registry reference again.
func (b *backend) createPinnedContainer(ctx context.Context, cfg core.Config, args []string, name, leaseID, slug, digest string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	retained := func(cause error) (string, error) {
		return "", &retainedImageContainerError{name: name, leaseID: leaseID, cause: cause}
	}
	result, err := b.imageControl(ctx, cfg, args, 10*time.Minute)
	if err != nil {
		return retained(err)
	}
	if strings.TrimSpace(result.Stdout) != name {
		return retained(exit(5, "Apple Container create did not confirm the requested target"))
	}
	observed, err := b.inspectImageContainer(ctx, cfg, name)
	if err != nil {
		return retained(err)
	}
	if !ownedStoppedImageContainer(observed.container, cfg.AppleContainer.Image, leaseID, slug) {
		return retained(exit(5, "Apple Container created target has incomplete or unexpected ownership/image metadata"))
	}
	if observed.container.Configuration.Image.Descriptor.Digest != digest {
		cause := exit(5, "Apple Container created image digest differs from the reviewed default; bootstrap refused")
		if err := b.rollbackImageContainer(cfg, observed, leaseID, slug); err != nil {
			return retained(errors.Join(cause, err))
		}
		return "", cause
	}
	err = core.WithDurableLeaseClaimLock(leaseID, func(_ *core.LeaseClaim, exists bool, _ func() error) error {
		if exists {
			return exit(2, "Apple Container claim appeared before verified image start")
		}
		fresh, err := b.inspectImageContainer(ctx, cfg, name)
		if err != nil {
			return err
		}
		if !ownedStoppedImageContainer(fresh.container, cfg.AppleContainer.Image, leaseID, slug) || !reflect.DeepEqual(fresh.configuration, observed.configuration) {
			return exit(2, "Apple Container configuration changed before verified image start")
		}
		_, err = b.imageControl(ctx, cfg, []string{"start", name}, 2*time.Minute)
		return err
	})
	if err != nil {
		return retained(err)
	}
	return name, nil
}

func (b *backend) rollbackImageContainer(cfg core.Config, observed imageContainerObservation, leaseID, slug string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return core.CleanupLeaseClaimIfUnchangedAfter(leaseID, core.LeaseClaim{}, false, func() error {
		name := observed.container.id()
		fresh, err := b.inspectImageContainer(ctx, cfg, name)
		if err != nil {
			return err
		}
		if !ownedStoppedImageContainer(fresh.container, cfg.AppleContainer.Image, leaseID, slug) || !reflect.DeepEqual(fresh.configuration, observed.configuration) {
			return exit(2, "Apple Container created target changed; refusing image rollback")
		}
		if _, err := b.imageControl(ctx, cfg, []string{"delete", "--force", name}, 30*time.Second); err != nil {
			return err
		}
		result, err := b.imageControl(ctx, cfg, []string{"ls", "--all", "--format", "json"}, 30*time.Second)
		if err != nil {
			return err
		}
		var containers []inspectContainer
		if strings.TrimSpace(result.Stdout) == "null" || json.Unmarshal([]byte(result.Stdout), &containers) != nil {
			return exit(5, "Apple Container image rollback inventory is invalid")
		}
		for _, c := range containers {
			if c.id() == "" || c.id() == name {
				return exit(5, "Apple Container image rollback absence is unconfirmed")
			}
		}
		return nil
	})
}

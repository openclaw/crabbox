package localcontainer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

const servedPreparationRecipe = "v1"

type servedImage struct {
	ID     string `json:"Id"`
	Config struct {
		Cmd        []string
		Entrypoint []string
		User       string
		Env        []string
	}
}

func (r *ServedRuntime) inspectImage(ctx context.Context, ref string) (servedImage, error) {
	result, err := r.b.docker(ctx, []string{"image", "inspect", ref}, nil, nil)
	if err != nil {
		return servedImage{}, err
	}
	var images []servedImage
	if err := json.Unmarshal([]byte(result.Stdout), &images); err != nil {
		return servedImage{}, err
	}
	if len(images) != 1 || images[0].ID == "" {
		return servedImage{}, fmt.Errorf("missing image identity for %s", ref)
	}
	return images[0], nil
}

func servedPreparedTag(baseID string) string {
	sum := sha256.Sum256([]byte(baseID + "\n" + bootstrapPackages + "\n" + servedPreparationRecipe))
	return fmt.Sprintf("crabbox-serve-prepared:%x", sum[:8])
}

func (r *ServedRuntime) Prepare(ctx context.Context) error {
	if err := r.EnsureImage(ctx); err != nil {
		return err
	}
	r.StartPreparation(ctx)
	r.WaitPreparation()
	return nil
}

// EnsureImage retains startup's fatal configured-image validation, without
// making reconciliation wait for optional package installation.
func (r *ServedRuntime) EnsureImage(ctx context.Context) error {
	r.imageMu.Lock()
	r.preparedImageID = ""
	r.baseImageID = ""
	r.baseImage = servedImage{}
	r.imageMu.Unlock()
	base, err := r.inspectImage(ctx, r.Image())
	if err != nil {
		if _, err := r.b.docker(ctx, []string{"pull", r.Image()}, r.b.rt.Stderr, r.b.rt.Stderr); err != nil {
			return err
		}
		base, err = r.inspectImage(ctx, r.Image())
		if err != nil {
			return err
		}
	}
	r.imageMu.Lock()
	r.baseImageID, r.baseImage = base.ID, base
	r.imageMu.Unlock()
	return nil
}

// StartPreparation uses the agent lifetime, never a single lease's deadline.
// Only one preparation runs per agent; image selection remains nonblocking.
func (r *ServedRuntime) StartPreparation(ctx context.Context) {
	r.imageMu.Lock()
	if r.preparing || ctx.Err() != nil || r.baseImage.ID == "" {
		r.imageMu.Unlock()
		return
	}
	r.preparing = true
	r.preparationCtx = ctx
	r.preparationDone = make(chan struct{})
	base, done := r.baseImage, r.preparationDone
	r.imageMu.Unlock()
	go func() {
		bounded, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		id, err := r.prepareImage(bounded, base)
		if err != nil && r.b.rt.Stderr != nil {
			fmt.Fprintf(r.b.rt.Stderr, "warning: serve image preparation failed; leases will bootstrap at start: %s\n", strings.ReplaceAll(err.Error(), "\n", "; "))
		}
		r.imageMu.Lock()
		if err == nil {
			r.preparedImageID = id
		}
		r.preparing = false
		close(done)
		r.imageMu.Unlock()
	}()
}

func (r *ServedRuntime) WaitPreparation() {
	r.imageMu.RLock()
	done := r.preparationDone
	r.imageMu.RUnlock()
	if done != nil {
		<-done
	}
}

func (r *ServedRuntime) preparedImageMissing(ctx context.Context, id string) bool {
	result, err := r.b.docker(ctx, []string{"image", "inspect", id}, nil, nil)
	if err == nil {
		return false
	}
	message := strings.ToLower(result.Stderr + "\n" + err.Error())
	return strings.Contains(message, "no such image") || strings.Contains(message, "image not known")
}

func (r *ServedRuntime) cleanupPreparationContainer(name string) error {
	cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := r.b.docker(cleanup, []string{"rm", "-f", name}, nil, r.b.rt.Stderr)
	return err
}

func (r *ServedRuntime) prepareImage(ctx context.Context, base servedImage) (id string, err error) {
	probe := "for tool in " + bootstrapTools + "; do command -v \"$tool\" >/dev/null 2>&1 || { command -v apt-get >/dev/null 2>&1 && { echo prepare; exit 0; }; echo skip; exit 0; }; done; echo ready"
	probeName := "crabbox-serve-probe-" + strings.ToLower(rand.Text())
	result, err := r.b.docker(ctx, []string{"run", "--rm", "--name", probeName, "--entrypoint", "/bin/sh", base.ID, "-c", probe}, nil, r.b.rt.Stderr)
	if err != nil {
		return "", errors.Join(err, r.cleanupPreparationContainer(probeName))
	}
	switch strings.TrimSpace(result.Stdout) {
	case "ready", "skip":
		return "", nil
	case "prepare":
	default:
		return "", fmt.Errorf("unexpected serve image tool probe: %q", result.Stdout)
	}
	tag := servedPreparedTag(base.ID)
	if prepared, err := r.inspectImage(ctx, tag); err == nil {
		return prepared.ID, nil
	}
	name := "crabbox-serve-prepare-" + strings.ToLower(rand.Text())
	committed := false
	defer func() {
		err = errors.Join(err, r.cleanupPreparationContainer(name))
		if err != nil && committed {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, removeErr := r.b.docker(cleanup, []string{"image", "rm", tag}, nil, r.b.rt.Stderr)
			err = errors.Join(err, removeErr)
		}
	}()
	args := []string{"run", "--name", name}
	// Avoid overriding empty defaults: Docker commit merges empty --change values
	// back from the temporary container rather than clearing them.
	if base.Config.User != "" {
		args = append(args, "--user", "root")
	}
	if len(base.Config.Entrypoint) != 0 {
		args = append(args, "--entrypoint", "/bin/sh")
	}
	args = append(args, base.ID)
	if len(base.Config.Entrypoint) == 0 {
		args = append(args, "/bin/sh")
	}
	script := "set -eu; export DEBIAN_FRONTEND=noninteractive; apt-get update; apt-get install -y --no-install-recommends " + bootstrapPackages + "; rm -f /etc/ssh/ssh_host_*; rm -rf /var/lib/apt/lists/*"
	args = append(args, "-c", script)
	if _, err := r.b.docker(ctx, args, r.b.rt.Stderr, r.b.rt.Stderr); err != nil {
		return "", err
	}
	// A nil slice marshals as null, which Docker treats as a shell command.
	// Empty JSON arrays clear the command for ENTRYPOINT-only images.
	cmd, _ := json.Marshal(append([]string{}, base.Config.Cmd...))
	entrypoint, _ := json.Marshal(append([]string{}, base.Config.Entrypoint...))
	args = []string{"commit", "--change", "CMD " + string(cmd)}
	args = append(args, "--change", "ENTRYPOINT "+string(entrypoint))
	if base.Config.User != "" {
		args = append(args, "--change", "USER "+base.Config.User)
	}
	for _, label := range []string{"crabbox.serve.prepared=true", "crabbox.serve.base-reference=" + strconv.Quote(r.Image()), "crabbox.serve.base-image-id=" + strconv.Quote(base.ID)} {
		args = append(args, "--change", "LABEL "+label)
	}
	args = append(args, name, tag)
	committed = true
	if _, err := r.b.docker(ctx, args, nil, r.b.rt.Stderr); err != nil {
		return "", err
	}
	prepared, err := r.inspectImage(ctx, tag)
	if err != nil {
		return "", err
	}
	if !slices.Equal(base.Config.Cmd, prepared.Config.Cmd) || !slices.Equal(base.Config.Entrypoint, prepared.Config.Entrypoint) || base.Config.User != prepared.Config.User || !slices.Equal(base.Config.Env, prepared.Config.Env) {
		return "", fmt.Errorf("serve prepared image did not preserve base CMD, entrypoint, user, and environment")
	}
	return prepared.ID, nil
}

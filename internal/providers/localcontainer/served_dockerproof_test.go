//go:build dockerproof

package localcontainer

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"golang.org/x/crypto/ssh"
)

func TestServedPreparedDockerProof(t *testing.T) {
	if os.Getenv("CRABBOX_SERVE_DOCKER_PROOF") != "1" {
		t.Skip("set CRABBOX_SERVE_DOCKER_PROOF=1 to measure local Docker startup")
	}
	const base = "docker.io/library/ubuntu@sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517"
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	cfg := core.BaseConfig()
	cfg.LocalContainer.Runtime = "docker"
	cfg.LocalContainer.Image = base
	rt := core.RuntimeForProviderOperation(os.Stderr)
	r := NewServedRuntime(cfg, rt)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	measure := func(label string) {
		t.Helper()
		id := fmt.Sprintf("cbx_%012x", time.Now().UnixNano()&0xffffffffffff)
		start := time.Now()
		c, err := r.Create(ctx, id, string(ssh.MarshalAuthorizedKey(public)), 1, 1024, base, nil, ServedContainerOptions{Deadline: time.Now().Add(5 * time.Minute).Unix(), Labels: map[string]string{"provider": "serve", "crabbox.serve": "true", "crabbox.serve.machine": "docker-proof"}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", c.ID).Run() })
		var key string
		for {
			key, err = r.HostKey(ctx, c)
			if err == nil && key != "" {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Logf("MEASUREMENT %s create-to-host-key=%s", label, time.Since(start))
		if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key)); err != nil {
			t.Fatal(err)
		}
		if keys[key] {
			t.Fatal("served containers reused a host key")
		}
		keys[key] = true
		// A readable key is the agent's readiness signal. Also prove that the
		// bootstrap finishes and sshd serves its banner on the published port.
		for {
			conn, dialErr := net.DialTimeout("tcp", net.JoinHostPort(c.Host, c.Port), time.Second)
			if dialErr == nil {
				_ = conn.SetReadDeadline(time.Now().Add(time.Second))
				banner, readErr := bufio.NewReader(conn).ReadString('\n')
				_ = conn.Close()
				if readErr == nil && strings.HasPrefix(banner, "SSH-2.0-") {
					break
				}
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Logf("MEASUREMENT %s create-to-SSH-banner=%s", label, time.Since(start))
		evidence, err := r.Evidence(ctx, c)
		if err != nil || evidence.ConfiguredReference != base {
			t.Fatalf("evidence=%+v error=%v", evidence, err)
		}
		if r.preparedImageID != "" && evidence.RuntimeImageID != r.preparedImageID {
			t.Fatalf("incorrect runtime image: %+v", evidence)
		}
		if evidence.RepositoryDigestStatus != "available" || len(evidence.RepositoryDigests) == 0 {
			t.Fatalf("missing base digests: %+v", evidence)
		}
		if err := r.Remove(ctx, c, "docker-proof"); err != nil {
			t.Fatal(err)
		}
	}
	measure("stock")
	baseImage, err := r.inspectImage(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	tag := servedPreparedTag(baseImage.ID)
	_, cachedErr := r.inspectImage(ctx, tag)
	start := time.Now()
	if err := r.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("MEASUREMENT preparation=%s reused=%t", time.Since(start), cachedErr == nil)
	if r.preparedImageID == "" {
		t.Fatal("stock image was not prepared")
	}
	if cachedErr != nil {
		t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", tag).Run() })
	}
	// Preparation must not bake host keys into the reusable image.
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", r.preparedImageID, "/bin/sh", "-c", "test -z \"$(find /etc/ssh -name 'ssh_host_*' -print)\"").CombinedOutput()
	if err != nil {
		t.Fatalf("prepared image retained host keys: %v: %s", err, out)
	}
	measure("prepared-1")
	measure("prepared-2")
	restart := NewServedRuntime(cfg, rt)
	start = time.Now()
	if err := restart.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if restart.preparedImageID != r.preparedImageID {
		t.Fatal("restart did not reuse the prepared image")
	}
	t.Logf("MEASUREMENT restart-preparation=%s", time.Since(start))
}

func TestServedPreparedEmptyCommandDockerProof(t *testing.T) {
	if os.Getenv("CRABBOX_SERVE_DOCKER_PROOF") != "1" {
		t.Skip("set CRABBOX_SERVE_DOCKER_PROOF=1 to verify empty commands with Docker")
	}
	const pinned = "docker.io/library/ubuntu@sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517"
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	cleanup := func(args ...string) {
		t.Helper()
		t.Cleanup(func() {
			bounded, stop := context.WithTimeout(context.Background(), 30*time.Second)
			defer stop()
			_ = exec.CommandContext(bounded, "docker", args...).Run()
		})
	}
	seed := docker("create", pinned)
	cleanup("rm", "-f", seed)
	// Setting ENTRYPOINT through commit clears the inherited Ubuntu CMD.
	baseID := docker("commit", "--change", `ENTRYPOINT ["/usr/bin/env"]`, seed)
	cleanup("image", "rm", baseID)
	cfg := core.BaseConfig()
	cfg.LocalContainer.Runtime = "docker"
	cfg.LocalContainer.Image = baseID
	r := NewServedRuntime(cfg, core.RuntimeForProviderOperation(os.Stderr))
	base, err := r.inspectImage(ctx, baseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Config.Cmd) != 0 || !reflect.DeepEqual(base.Config.Entrypoint, []string{"/usr/bin/env"}) {
		t.Fatalf("not an ENTRYPOINT-only base: %+v", base.Config)
	}
	temporary := docker("create", "--entrypoint", "/bin/sh", baseID, "-c", "echo temporary-command")
	cleanup("rm", "-f", temporary)
	for _, change := range []string{"CMD null", "CMD []", "ENTRYPOINT []", "restore-both"} {
		args := []string{"commit"}
		if change == "restore-both" {
			args = append(args, "--change", "CMD []", "--change", `ENTRYPOINT ["/usr/bin/env"]`)
		} else {
			args = append(args, "--change", change)
		}
		image := docker(append(args, temporary)...)
		cleanup("image", "rm", image)
		observed, err := r.inspectImage(ctx, image)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("Docker %s: Cmd=%v Entrypoint=%v", change, observed.Config.Cmd, observed.Config.Entrypoint)
		if change == "CMD []" && len(observed.Config.Cmd) != 0 {
			t.Fatal("CMD [] did not clear the temporary command")
		}
		if change == "restore-both" && !reflect.DeepEqual(observed.Config, base.Config) {
			t.Fatalf("explicit CMD/ENTRYPOINT restoration changed config: %+v", observed.Config)
		}
	}
	tag := servedPreparedTag(baseID)
	cleanup("image", "rm", tag)
	start := time.Now()
	if err := r.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if r.preparedImageID == "" {
		t.Fatal("ENTRYPOINT-only preparation unexpectedly fell back")
	}
	prepared, err := r.inspectImage(ctx, r.preparedImageID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prepared.Config, base.Config) {
		t.Fatalf("config changed: got=%+v want=%+v", prepared.Config, base.Config)
	}
	t.Logf("ENTRYPOINT-only preparation preserved Cmd=%v Entrypoint=%v User=%q Env=%v in %s", prepared.Config.Cmd, prepared.Config.Entrypoint, prepared.Config.User, prepared.Config.Env, time.Since(start))
	if got := docker("run", "--rm", r.preparedImageID, "echo", "entrypoint-preserved"); got != "entrypoint-preserved" {
		t.Fatal(got)
	}
}

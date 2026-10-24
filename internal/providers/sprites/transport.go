package sprites

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Both the bootstrap CLI and SSH's ProxyCommand must use the same endpoint and
// credential as the API client, regardless of saved CLI context or env aliases.
// These values stay in child-process environments, never argv or lease claims.
func (b *spritesBackend) cliEnvironment() (map[string]string, error) {
	endpoint, _, err := validateSpritesAPIURL(core.Blank(b.cfg.Sprites.APIURL, "https://api.sprites.dev"))
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"SPRITE_TOKEN":    strings.TrimSpace(b.cfg.Sprites.Token),
		"SPRITE_URL":      endpoint,
		"SPRITES_API_URL": endpoint,
	}, nil
}

func (b *spritesBackend) sshTarget(name, keyPath string) (core.SSHTarget, error) {
	target := spritesSSHTarget(name, keyPath)
	env, err := b.cliEnvironment()
	if err != nil {
		return core.SSHTarget{}, err
	}
	target.ChildEnv = env
	// An existing master may have authenticated through a different CLI context.
	target.NoControlMaster = true
	return target, nil
}

func (b *spritesBackend) runSprite(ctx context.Context, args []string, stdout, stderr io.Writer) (core.LocalCommandResult, error) {
	overrides, err := b.cliEnvironment()
	if err != nil {
		return core.LocalCommandResult{ExitCode: 2}, err
	}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, overridden := overrides[strings.ToUpper(name)]; !overridden {
			env = append(env, entry)
		}
	}
	for _, name := range []string{"SPRITE_TOKEN", "SPRITE_URL", "SPRITES_API_URL"} {
		env = append(env, name+"="+overrides[name])
	}
	return b.rt.Exec.Run(ctx, core.LocalCommandRequest{Name: "sprite", Args: args, Env: env, Stdout: stdout, Stderr: stderr})
}

func effectiveSpriteHoldTimeout(requested, configured time.Duration) time.Duration {
	if requested <= 0 {
		requested = configured
	}
	if requested <= 0 {
		requested = 30 * time.Minute
	}
	if requested < time.Minute {
		return time.Minute
	}
	if requested > time.Hour {
		return time.Hour
	}
	return requested.Truncate(time.Second)
}

func (b *spritesBackend) holdSpriteTask(ctx context.Context, name string, expire time.Duration) error {
	seconds := int(effectiveSpriteHoldTimeout(expire, b.cfg.IdleTimeout).Seconds())
	script := "curl -fsS --unix-socket /.sprite/api.sock -H 'Content-Type: application/json' -o /dev/null -X PUT http://sprite/v1/tasks/crabbox-lease -d '{\"expire\":" + strconv.Itoa(seconds) + "}'"
	result, err := b.runSprite(ctx, []string{"exec", "-s", name, "--", "sh", "-c", script}, nil, b.rt.Stderr)
	if err != nil {
		return fmt.Errorf("sprites hold task failed (exit code %d): %w", result.ExitCode, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("sprites hold task exited with code %d", result.ExitCode)
	}
	return nil
}

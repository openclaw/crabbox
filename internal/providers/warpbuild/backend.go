package warpbuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type backend struct {
	cfg core.Config
	rt  core.Runtime
}

func (b *backend) Spec() core.ProviderSpec { return Provider{}.Spec() }

func verifyIdentity(claim core.LeaseClaim, item sandbox) error {
	if claim.Provider != providerName || claim.ProviderScope != apiScope || claim.CloudID != item.Name || claim.CloudImmutableID == "" || claim.CloudImmutableID != item.ID || claim.Labels["warpbuild_created"] == "" || claim.Labels["warpbuild_created"] != item.Created {
		return core.Exit(4, "WarpBuild sandbox identity differs from the recorded lease; refusing operation")
	}
	return nil
}

func (b *backend) verify(ctx context.Context, c *client, claim core.LeaseClaim) (sandbox, error) {
	item, err := c.info(ctx, claim.CloudID)
	if err != nil {
		return sandbox{}, err
	}
	return item, verifyIdentity(claim, item)
}

func server(item sandbox, claim core.LeaseClaim) core.Server {
	view := shared.SandboxLeaseView(providerName, core.TargetMacOS, claim, item.Name, item.Name, item.State)
	for key, value := range shared.ClaimLifecycleLabels(claim) {
		view.Labels[key] = value
	}
	view.Labels["state"] = item.State
	view.ImmutableID = item.ID
	view.Labels["warpbuild_created"] = item.Created
	view.ServerType.Name = item.Size
	return view
}

func (b *backend) create(ctx context.Context, c *client, repo core.Repo, reclaim bool, requestedSlug string) (core.LeaseClaim, error) {
	if strings.TrimSpace(repo.Root) == "" {
		return core.LeaseClaim{}, core.Exit(2, "provider=warpbuild requires a repository workspace")
	}
	leaseID := core.NewLeaseID()
	slug, err := core.AllocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	name := "crabbox-" + strings.TrimPrefix(leaseID, "cbx_")
	if err := c.create(ctx, name); err != nil {
		return core.LeaseClaim{}, fmt.Errorf("%w; creation may be incomplete: inspect with wb sbx info %s before cleanup", err, name)
	}
	item, err := c.info(ctx, name)
	if err != nil {
		return core.LeaseClaim{}, fmt.Errorf("%w; retain sandbox %s for identity reconciliation", err, name)
	}
	view := server(item, core.LeaseClaim{LeaseID: leaseID, Slug: slug})
	claim, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfterContext(ctx, leaseID, slug, b.cfg, apiScope, view, core.SSHTarget{}, repo.Root, b.cfg.IdleTimeout, reclaim, core.LeaseClaim{}, false, func() error {
		current, err := c.info(ctx, name)
		if err != nil {
			return err
		}
		return verifyIdentity(core.LeaseClaim{Provider: providerName, ProviderScope: apiScope, CloudID: name, CloudImmutableID: item.ID, Labels: view.Labels}, current)
	})
	if err != nil {
		return core.LeaseClaim{}, fmt.Errorf("%w; retain sandbox %s (id=%s) for claim recovery", err, name, item.ID)
	}
	return claim, nil
}

func (b *backend) resolve(ctx context.Context, id string) (core.LeaseClaim, error) {
	claim, ok, exact, err := core.ResolveLeaseClaimForProviderScopeWithExactContext(ctx, id, providerName, apiScope)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if !ok || core.IsCanonicalLeaseID(id) && (!exact || claim.LeaseID != id) {
		return core.LeaseClaim{}, core.Exit(4, "WarpBuild lease %q has no matching local claim", id)
	}
	return claim, nil
}

func (b *backend) stop(ctx context.Context, c *client, claim core.LeaseClaim) error {
	return core.CleanupLeaseClaimIfUnchangedAfterContext(ctx, claim.LeaseID, claim, true, func() error {
		if _, err := b.verify(ctx, c, claim); errors.Is(err, errSandboxNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		if _, err := c.control(ctx, "sbx", "kill", claim.CloudID, "--yes"); err != nil {
			return err
		}
		// Inventory absence after a successful delete establishes completion.
		items, err := c.list(ctx)
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.Name == claim.CloudID {
				return core.Exit(5, "WarpBuild deletion is not confirmed; retain the lease claim")
			}
		}
		return nil
	})
}

func (b *backend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	c, err := newClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	started := core.ClockNow(b.rt.Clock)
	claim, err := b.create(ctx, c, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=warpbuild sandbox=%s\n", claim.LeaseID, claim.Slug, claim.CloudID)
	fmt.Fprintln(b.rt.Stderr, "WarpBuild has a native seven-day lifetime; Crabbox TTL and idle settings do not schedule provider deletion. Stop this lease explicitly when finished.")
	return shared.CompleteWarmup(b.rt, req.TimingJSON, shared.WarmupCompletion{Provider: providerName, LeaseID: claim.LeaseID, Slug: claim.Slug, Total: core.ClockNow(b.rt.Clock).Sub(started)})
}

func (b *backend) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	var c *client
	var claim core.LeaseClaim
	session := func(unlock func()) shared.DelegatedSandbox {
		return shared.DelegatedSandbox{LeaseID: claim.LeaseID, Slug: claim.Slug, Unlock: unlock, CleanupCommand: "crabbox stop --provider warpbuild " + core.ShellQuote(claim.LeaseID)}
	}
	return shared.RunDelegatedSandbox(ctx, req, shared.DelegatedSandboxLifecycle{
		Provider: providerName, Runtime: b.rt, Workdir: b.cfg.WarpBuild.Workdir, CleanupTimeout: 45 * time.Second,
		Preflight: func(context.Context) error {
			if err := core.RejectDelegatedSyncOptionsForSpec(b.Spec(), req); err != nil {
				return err
			}
			var err error
			c, err = newClient(b.cfg, b.rt)
			return err
		},
		Workspace: func() shared.SandboxWorkspace {
			workspace := core.NewArchiveWorkspace(b.cfg, b.rt, req, providerName, b.cfg.WarpBuild.Workdir)
			workspace.Upload = func(ctx context.Context, path string, source io.Reader) error {
				return core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, claim, func() error {
					if _, err := b.verify(ctx, c, claim); err != nil {
						return err
					}
					return c.upload(ctx, claim.CloudID, path, source)
				})
			}
			workspace.Exec = func(ctx context.Context, script string) error {
				return core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, claim, func() error {
					if _, err := b.verify(ctx, c, claim); err != nil {
						return err
					}
					return c.shell(ctx, claim.CloudID, script)
				})
			}
			workspace.CleanWorkdir = func(path string) (string, error) {
				return shared.CleanPOSIXWorkspacePath("warpbuild workdir", path, "/Users", "/Users/runner", "/Applications", "/Library", "/System")
			}
			return workspace
		},
		Acquire: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			claim, err = b.create(ctx, c, req.Repo, req.Reclaim, req.RequestedSlug)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			unlock, err := shared.LockLeaseOperation(ctx, providerName, claim.LeaseID)
			return session(unlock), err
		},
		Resolve: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			claim, err = b.resolve(ctx, req.ID)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			unlock, err := shared.LockLeaseOperation(ctx, providerName, claim.LeaseID)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			if err := shared.AuthorizeClaimActivity(claim); err != nil {
				unlock()
				return shared.DelegatedSandbox{}, err
			}
			item, err := b.verify(ctx, c, claim)
			if err == nil {
				claim, err = core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfterContext(ctx, claim.LeaseID, claim.Slug, b.cfg, apiScope, server(item, claim), core.SSHTarget{}, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim, claim, true, func() error { _, err := b.verify(ctx, c, claim); return err })
			}
			if err != nil {
				unlock()
				return shared.DelegatedSandbox{}, err
			}
			return session(unlock), nil
		},
		AdmitReuse: func(ctx context.Context) error {
			item, err := b.verify(ctx, c, claim)
			if err != nil {
				return err
			}
			if strings.ToLower(item.State) != "ready" && strings.ToLower(item.State) != "running" {
				return core.Exit(4, "WarpBuild sandbox is %s; resume it before running", item.State)
			}
			return shared.AuthorizeClaimActivity(claim)
		},
		Command: func(context.Context) (shared.DelegatedSandboxCommand, error) {
			intent, err := core.ParseCommandIntent(req.Command, req.ShellMode, req.CommandLiteralArgs)
			if err != nil {
				return shared.DelegatedSandboxCommand{}, err
			}
			return shared.DelegatedSandboxCommand{Text: strings.Join(req.Command, " "), OutputScope: core.RunOutputProvider, Run: func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
				var code int
				err := core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, claim, func() error {
					if _, err := b.verify(ctx, c, claim); err != nil {
						return err
					}
					var err error
					code, err = c.run(ctx, claim.CloudID, b.cfg.WarpBuild.Workdir, intent.Argv("/bin/bash", "-c"), req.Env, stdout, stderr)
					return err
				})
				if ctx.Err() != nil {
					fmt.Fprintf(b.rt.Stderr, "WarpBuild disconnected; remote command termination is unconfirmed. Stop lease %s to terminate its sandbox.\n", claim.LeaseID)
				}
				return code, err
			}}, nil
		},
		Retained: func(context.Context) error {
			fmt.Fprintln(b.rt.Stderr, "WarpBuild lease retained; stop it explicitly. Crabbox TTL and idle settings do not schedule provider deletion.")
			return nil
		},
		Cleanup: func(ctx context.Context) error { return b.stop(ctx, c, claim) },
	})
}

func (b *backend) Stop(ctx context.Context, req core.StopRequest) error {
	c, err := newClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claim, err := b.resolve(ctx, req.ID)
	if err != nil {
		return err
	}
	unlock, err := shared.LockLeaseOperation(ctx, providerName, claim.LeaseID)
	if err != nil {
		return err
	}
	defer unlock()
	return b.stop(ctx, c, claim)
}

func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	c, err := newClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	items, err := c.list(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := shared.IndexProviderClaims(providerName, func(claim core.LeaseClaim) string {
		if claim.ProviderScope == apiScope {
			return claim.CloudID
		}
		return ""
	})
	if err != nil {
		return nil, err
	}
	views := make([]core.LeaseView, 0)
	for _, row := range items {
		claim, ok := claims[row.Name]
		if !ok {
			continue
		}
		item, err := b.verify(ctx, c, claim)
		if err != nil {
			return nil, err
		}
		views = append(views, server(item, claim))
	}
	return views, nil
}

func (b *backend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	c, err := newClient(b.cfg, b.rt)
	if err != nil {
		return core.StatusView{}, err
	}
	claim, err := b.resolve(ctx, req.ID)
	if err != nil {
		return core.StatusView{}, err
	}
	return shared.PollStatus(ctx, req, func() time.Time { return core.ClockNow(b.rt.Clock) }, func(ctx context.Context) (core.StatusView, bool, error) {
		item, err := b.verify(ctx, c, claim)
		if err != nil {
			return core.StatusView{}, false, err
		}
		state := strings.ToLower(item.State)
		return core.StatusView{ID: claim.LeaseID, Slug: claim.Slug, Provider: providerName, TargetOS: core.TargetMacOS, State: state, ServerID: item.ID, Ready: state == "ready" || state == "running"}, state == "ready" || state == "running" || state == "paused" || state == "failed", nil
	}, nil)
}

func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	c, err := newClient(b.cfg, b.rt)
	if err != nil {
		return core.DoctorResult{}, err
	}
	items, err := c.list(ctx)
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.InventoryDoctorResult(providerName, len(items)), nil
}

func (b *backend) transition(ctx context.Context, id, action string) error {
	c, err := newClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claim, err := b.resolve(ctx, id)
	if err != nil {
		return err
	}
	unlock, err := shared.LockLeaseOperation(ctx, providerName, claim.LeaseID)
	if err != nil {
		return err
	}
	defer unlock()
	return core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, claim, func() error {
		if err := shared.AuthorizeClaimActivity(claim); err != nil {
			return err
		}
		if _, err := b.verify(ctx, c, claim); err != nil {
			return err
		}
		waitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		_, err := c.call(waitCtx, nil, nil, nil, "sbx", action, claim.CloudID, "--wait", "150")
		if err != nil {
			return err
		}
		item, err := b.verify(ctx, c, claim)
		if err != nil {
			return err
		}
		want := "paused"
		if action == "resume" {
			want = "ready"
		}
		if strings.ToLower(item.State) != want {
			return core.Exit(5, "WarpBuild %s did not complete: sandbox state is %s", action, item.State)
		}
		return nil
	})
}
func (b *backend) Pause(ctx context.Context, req core.PauseRequest) error {
	return b.transition(ctx, req.ID, "pause")
}
func (b *backend) Resume(ctx context.Context, req core.ResumeRequest) error {
	return b.transition(ctx, req.ID, "resume")
}

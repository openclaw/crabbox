package ssh

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/openclaw/crabbox/internal/atomicfile"
	core "github.com/openclaw/crabbox/internal/cli"
)

// A host record outlives its leases: changing a host's endpoint or hook contract
// cannot silently give another configuration authority over an existing host.
type powerHostRecord struct {
	Stopped    bool                      `json:"stopped"`
	Version    int                       `json:"version"`
	Identity   string                    `json:"identity"`
	Contract   string                    `json:"contract"`
	References map[string]powerReference `json:"references"`
}
type powerReference struct {
	Token    string `json:"token"`
	RepoRoot string `json:"repoRoot"`
	State    string `json:"state"`
}
type powerHost struct {
	path     string
	identity string
	contract string
	record   powerHostRecord
}

func (b *staticLeaseBackend) hasPowerHooks() bool {
	return len(b.Cfg.Static.StartCommand) != 0 || len(b.Cfg.Static.StopCommand) != 0
}

func (b *staticLeaseBackend) powerIdentity() (host, identity, contract string, err error) {
	if !b.Cfg.Static.Power.Dedicated {
		return "", "", "", core.Exit(2, "static power hooks require static.power.dedicated: true; this controller must exclusively own the host")
	}
	_, target, _, err := core.StaticLease(b.Cfg)
	if err != nil {
		return "", "", "", err
	}
	ip := net.ParseIP(target.Host)
	if ip == nil {
		return "", "", "", core.Exit(2, "static power hooks refuse hostname aliases: configure static.host as one canonical IP literal and use the same user/port for every lease")
	}
	port, err := strconv.Atoi(target.Port)
	if err != nil || port < 1 || port > 65535 {
		return "", "", "", core.Exit(2, "static power hooks require an explicit valid static.port")
	}
	host = ip.String()
	identity = target.User + "@" + net.JoinHostPort(host, strconv.Itoa(port)) + "#" + b.Cfg.Static.Power.HostID
	data, _ := json.Marshal(struct {
		Identity    string
		Start, Stop []string
	}{identity, b.Cfg.Static.StartCommand, b.Cfg.Static.StopCommand})
	sum := sha256.Sum256(data)
	return host, identity, hex.EncodeToString(sum[:]), nil
}

func powerRecordPath(host string) (string, error) {
	dir, err := core.CrabboxStateDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(host))
	return filepath.Join(dir, "ssh-power", hex.EncodeToString(sum[:])+".json"), nil
}

func (b *staticLeaseBackend) lockPowerHost(ctx context.Context) (*powerHost, func(), error) {
	host, identity, contract, err := b.powerIdentity()
	if err != nil {
		return nil, nil, err
	}
	unlock, err := lockStaticHostPower(ctx, host)
	if err != nil {
		return nil, nil, err
	}
	if hostID := b.Cfg.Static.Power.HostID; hostID != "" {
		idUnlock, idErr := lockStaticHostPower(ctx, "host-id:"+hostID)
		if idErr != nil {
			unlock()
			return nil, nil, idErr
		}
		hostUnlock := unlock
		unlock = func() { idUnlock(); hostUnlock() }
		// A configured host ID may not create a second authority record through
		// another address of the same machine. Keep this binding durable too.
		idPath, idErr := powerRecordPath("host-id:" + hostID)
		if idErr == nil {
			binding := &powerHost{path: idPath, identity: identity, contract: contract}
			idErr = binding.read()
			if idErr == nil {
				idErr = binding.save()
			}
		}
		if idErr != nil {
			unlock()
			return nil, nil, idErr
		}
	}
	path, err := powerRecordPath(host)
	h := &powerHost{path: path, identity: identity, contract: contract}
	if err == nil {
		err = h.read()
	}
	if err == nil {
		err = h.retireReceipts()
	}
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return h, unlock, nil
}

func (h *powerHost) read() error {
	data, err := os.ReadFile(h.path)
	if errors.Is(err, os.ErrNotExist) {
		h.record = powerHostRecord{Version: 1, Identity: h.identity, Contract: h.contract, References: map[string]powerReference{}}
		return nil
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &h.record); err != nil {
		return fmt.Errorf("read static host custody: %w", err)
	}
	if h.record.Version != 1 || h.record.References == nil || h.record.Identity != h.identity || h.record.Contract != h.contract {
		return core.Exit(4, "static host power identity or hook contract changed; restore the original trusted configuration before recovery")
	}
	for id, ref := range h.record.References {
		if id == "" || ref.Token == "" || (ref.State != "starting" && ref.State != "active" && ref.State != "pending-stop" && ref.State != "stopped") {
			return core.Exit(4, "invalid static host custody; refusing power hooks")
		}
	}
	return nil
}

func (h *powerHost) retireReceipts() error {
	changed := false
	for id, ref := range h.record.References {
		if ref.State != "stopped" {
			continue
		}
		_, exists, err := core.ReadLeaseClaimWithPresence(id)
		if err != nil {
			return err
		}
		if !exists {
			delete(h.record.References, id)
			changed = true
		}
	}
	if changed {
		return h.save()
	}
	return nil
}

func (h *powerHost) save() error {
	// Reuse the claim namespace durability boundary before adding this adapter's
	// sidecar directory, then sync both the record and its parent entry.
	if err := core.EnsureCrabboxClaimNamespaceDurable(); err != nil {
		return err
	}
	dir := filepath.Dir(h.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := syncPowerDirectory(filepath.Dir(dir)); err != nil {
		return err
	}
	data, err := json.Marshal(h.record)
	if err != nil {
		return err
	}
	if err = atomicfile.WritePrivate(h.path, ".power-*", data, os.Rename); err != nil {
		return err
	}
	return syncPowerDirectory(dir)
}
func syncPowerDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (b *staticLeaseBackend) rejectPowerCustody(ctx context.Context) error {
	// Plain static operation cannot shed custody by clearing configured hooks.
	host := strings.TrimSpace(b.Cfg.Static.Host)
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	path, err := powerRecordPath(host)
	if err != nil {
		return err
	}
	if _, err = os.Stat(path); err == nil {
		return core.Exit(4, "static host has a power contract; restore its dedicated-host configuration")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return ctx.Err()
}

func (b *staticLeaseBackend) acquirePowered(ctx context.Context, req core.AcquireRequest) (_ core.LeaseTarget, resultErr error) {
	if err := core.ValidateLocalCommandProcessGroupJoin(ctx); err != nil {
		return core.LeaseTarget{}, err
	}
	h, unlock, err := b.lockPowerHost(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	defer unlock()
	cfg := b.Cfg
	server, target, id, err := core.StaticLease(cfg)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.RequestedSlug != "" {
		slug, err := core.AllocateClaimLeaseSlug(id, req.RequestedSlug)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		cfg.Static.Name = slug
		server, target, id, err = core.StaticLease(cfg)
		if err != nil {
			return core.LeaseTarget{}, err
		}
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(id); err != nil {
		return core.LeaseTarget{}, err
	} else if exists {
		return core.LeaseTarget{}, core.Exit(4, "static power lease %s already has custody; overlapping acquisitions of the same ID are refused; stop it first or choose a distinct static.id", id)
	}
	for leaseID, ref := range h.record.References {
		if leaseID == id || ref.State != "active" {
			return core.LeaseTarget{}, core.Exit(4, "static host has unresolved custody for %s (%s); retry crabbox stop --provider ssh --id %s", leaseID, ref.State, leaseID)
		}
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	host := net.ParseIP(cfg.Static.Host)
	for _, existing := range claims {
		if existing.Provider != staticProvider {
			continue
		}
		ip := net.ParseIP(existing.StaticHost)
		if ip != nil && !ip.Equal(host) {
			continue
		}
		ref, tracked := h.record.References[existing.LeaseID]
		if !tracked || ref.Token != existing.Labels["static_power_token"] {
			return core.LeaseTarget{}, core.Exit(4, "static host has an untracked lease %s (or unresolved hostname alias); stop existing unpowered leases before enabling power hooks", existing.LeaseID)
		}
	}
	if req.Repo.Root == "" {
		return core.LeaseTarget{}, core.Exit(2, "static power acquisition requires repository ownership")
	}
	tokenBytes := make([]byte, 16)
	if _, err = rand.Read(tokenBytes); err != nil {
		return core.LeaseTarget{}, err
	}
	token := hex.EncodeToString(tokenBytes)
	first := len(h.record.References) == 0
	if first {
		h.record.Stopped = false
	}
	h.record.References[id] = powerReference{Token: token, RepoRoot: req.Repo.Root, State: "starting"}
	// Publish the reference before side effects. An interrupted publication is
	// recoverable by exact-ID forced stop, never silently adopted by acquisition.
	if err = h.save(); err != nil {
		return core.LeaseTarget{}, err
	}
	server.Labels["static_power_token"] = token
	server.Labels["static_power_identity"] = h.identity
	server.Labels["state"] = "power-starting"
	claim, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(id, core.ServerSlug(server), cfg, core.ProviderClaimScope(staticProvider, cfg), server, target, req.Repo.Root, cfg.IdleTimeout, false, core.LeaseClaim{}, false)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if first && len(cfg.Static.StartCommand) > 0 {
		if err = b.startStaticHost(ctx, id, target.Host); err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				h.record.References[id] = powerReference{Token: token, RepoRoot: req.Repo.Root, State: "pending-stop"}
				return core.LeaseTarget{}, errors.Join(err, h.save(), fmt.Errorf("start interrupted; custody retained; retry crabbox stop --provider ssh --id %s", id))
			}
			// An ordinary unsuccessful start grants no power-on custody.
			ref := h.record.References[id]
			ref.State = "stopped"
			if first {
				h.record.Stopped = true
			}
			h.record.References[id] = ref
			return core.LeaseTarget{}, errors.Join(err, b.finishPowerReference(context.WithoutCancel(ctx), h, id, claim))
		}
	}
	defer func() {
		if resultErr != nil {
			rollbackErr := b.finishPowerReference(context.WithoutCancel(ctx), h, id, claim)
			resultErr = errors.Join(resultErr, rollbackErr)
		}
	}()
	configurePoweredTarget(&target)
	route := architectureEndpoint(target)
	if err = waitForSSH(ctx, &target, b.RT.Stderr); err != nil {
		return core.LeaseTarget{}, err
	}
	lease := core.LeaseTarget{Server: server, SSH: target, LeaseID: id}
	if err = b.observeArchitecture(ctx, &lease); err != nil {
		return core.LeaseTarget{}, err
	}
	lease.Server.Labels["architecture_route"] = route
	lease.Server.Labels["state"] = "ready"
	updated, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(id, core.ServerSlug(server), cfg, core.ProviderClaimScope(staticProvider, cfg), lease.Server, target, req.Repo.Root, cfg.IdleTimeout, false, claim, true)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	claim = updated
	h.record.References[id] = powerReference{Token: token, RepoRoot: req.Repo.Root, State: "active"}
	if err = h.save(); err != nil {
		return core.LeaseTarget{}, err
	}
	core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
	b.rememberAcquiredLease(lease)
	return lease, nil
}

func (b *staticLeaseBackend) releasePowered(ctx context.Context, req core.ReleaseLeaseRequest) error {
	h, unlock, err := b.lockPowerHost(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	carried, exists, set := core.ServerLeaseClaimSnapshot(req.Lease.Server)
	if !set || !exists {
		return core.Exit(4, "static power release carries no claim snapshot; resolve the original lease before retrying stop")
	}
	if latest, ok := b.acquisitionLatestClaim(req.Lease.LeaseID, carried.Revision); ok {
		carried = latest
	}
	if carried.LeaseID != req.Lease.LeaseID || carried.Provider != staticProvider || carried.Labels["static_power_identity"] != h.identity {
		return core.Exit(4, "static power release identity does not match the host contract")
	}
	if err = b.finishPowerReference(ctx, h, req.Lease.LeaseID, carried, func() {
		if req.GuardedRemoteCleanup != nil {
			req.GuardedRemoteCleanup(ctx, req.Lease)
		}
	}); err != nil {
		return err
	}
	b.clearAcquiredLease(req.Lease.LeaseID)
	return nil
}

func (b *staticLeaseBackend) finishPowerReference(ctx context.Context, h *powerHost, id string, claim core.LeaseClaim, cleanup ...func()) error {
	ref, ok := h.record.References[id]
	if !ok || ref.Token != claim.Labels["static_power_token"] {
		return core.Exit(4, "static power reference changed; refusing stale release")
	}
	// The shared claim CAS holds through pending-state publication, the hook,
	// and successful custody retirement. Heartbeats cannot race the shutdown.
	err := core.RemoveLeaseClaimIfUnchangedAfter(id, claim, func() error {
		if len(h.record.References) == 1 && !h.record.Stopped {
			if ref.State == "active" {
				for _, action := range cleanup {
					action()
				}
			}
			ref.State = "pending-stop"
			h.record.References[id] = ref
			if err := h.save(); err != nil {
				return err
			}
			if len(b.Cfg.Static.StopCommand) > 0 {
				fmt.Fprintf(b.RT.Stderr, "stopping static host=%s lease=%s\n", b.Cfg.Static.Host, id)
				if err := b.runStaticPowerCommand(ctx, "static.stopCommand", b.Cfg.Static.StopCommand, id, b.Cfg.Static.Host); err != nil {
					return errors.Join(err, fmt.Errorf("pending-stop retained; retry crabbox stop --provider ssh --id %s", id))
				}
			}
		}
		if len(h.record.References) == 1 {
			h.record.Stopped = true
		}
		// The stopped receipt prevents repeating a successful hook if claim removal
		// fails. Other references remain active until their own guarded releases.
		ref.State = "stopped"
		h.record.References[id] = ref
		if err := h.save(); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	delete(h.record.References, id)
	return h.save()
}

// ReclaimAndStop is the existing explicit --force recovery surface. The trusted
// dedicated contract plus explicit acknowledgement never adopt an unknown host.
func (b *staticLeaseBackend) ReclaimAndStop(ctx context.Context, req core.StopRequest) error {
	if !b.hasPowerHooks() {
		return core.Exit(2, "provider=ssh does not support verified forced recovery without a dedicated power contract")
	}
	if !b.Cfg.Static.PowerAcknowledgeStop {
		return core.Exit(2, "forced static power recovery requires --static-power-acknowledge-stop; the stop hook can power off this dedicated host")
	}
	h, unlock, err := b.lockPowerHost(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	ref, ok := h.record.References[req.ID]
	if !ok {
		return core.Exit(4, "forced static power recovery requires an exact lease ID with retained host custody")
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.ID)
	if err != nil {
		return err
	}
	if !exists {
		// A crash between the durable host reservation and lease publication has
		// no claim to resolve. Rebuild only that exact retained reservation by CAS.
		cfg := b.Cfg
		cfg.Static.ID = req.ID
		server, target, id, err := core.StaticLease(cfg)
		if err != nil {
			return err
		}
		server.Labels["static_power_token"] = ref.Token
		server.Labels["static_power_identity"] = h.identity
		claim, err = core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(id, core.ServerSlug(server), cfg, core.ProviderClaimScope(staticProvider, cfg), server, target, ref.RepoRoot, cfg.IdleTimeout, false, core.LeaseClaim{}, false)
		if err != nil {
			return err
		}
	}
	if claim.Provider != staticProvider || claim.Labels["static_power_identity"] != h.identity {
		return core.Exit(4, "forced static power recovery claim identity mismatch")
	}
	return b.finishPowerReference(ctx, h, req.ID, claim)
}

func configurePoweredTarget(target *core.SSHTarget) {
	// Literal endpoints must not be retargeted through ambient SSH aliases or
	// fallback ports after the host power authority has been bound.
	target.FallbackPorts = []string{}
	target.SSHConfigFile = os.DevNull
	target.SSHConfigData = []byte("Host *\n  IdentitiesOnly yes\n")
	target.SSHConfigProxy = false
	target.ProxyCommand = ""
}

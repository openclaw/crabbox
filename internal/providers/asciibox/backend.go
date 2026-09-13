package asciibox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path"
	"regexp"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type backend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

func NewBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	cfg.TargetOS = targetLinux
	cfg.Network = networkPublic
	if cleaned, err := cleanWorkdir(workdir(cfg)); err == nil {
		cfg.WorkRoot = cleaned
	}
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

func (b *backend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	cfg, err := b.configForRun()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	client, err := newAPI(cfg, b.rt)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	slug, err := core.AllocateClaimLeaseSlug(leaseID, req.RequestedSlug)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	ttl := req.Options.TTL
	if ttl <= 0 {
		ttl = cfg.TTL
	}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s ttl=%s\n", providerName, leaseID, slug, core.Blank(ttl.String(), "-"))
	box, createErr := client.CreateBox(ctx, createRequest{TTL: ttl})
	if !concreteBoxID(box.createdID) || box.ID != box.createdID {
		if createErr != nil {
			return core.LeaseTarget{}, fmt.Errorf("ascii-box creation did not establish an original Box ID; retaining any resource: %w", createErr)
		}
		return core.LeaseTarget{}, core.Exit(2, "ascii-box creation did not establish an original Box ID; retaining any resource")
	}
	if createErr != nil {
		return core.LeaseTarget{}, errors.Join(createErr, b.rollbackBox(ctx, client, leaseID, box, core.LeaseClaim{}, false))
	}
	fresh, err := client.GetBox(ctx, box.ID)
	if err != nil {
		return core.LeaseTarget{}, fmt.Errorf("ascii-box created %s but could not verify its identity; resource retained: %w", box.ID, err)
	}
	if fresh.ID != box.ID || boxCreationTime(fresh) == "" || boxCreationTime(box) != "" && boxCreationTime(fresh) != boxCreationTime(box) {
		return core.LeaseTarget{}, core.Exit(2, "ascii-box created %s but its creation identity is missing or changed; resource retained", box.ID)
	}
	box = mergeBox(box, fresh)
	claim, err := publishBoxClaim(cfg, leaseID, slug, req.Repo.Root, box, req.Keep)
	if err != nil {
		return core.LeaseTarget{}, errors.Join(err, b.rollbackBox(ctx, client, leaseID, box, core.LeaseClaim{}, false))
	}
	var lease core.LeaseTarget
	err = core.WithLeaseClaimUnchanged(leaseID, claim, func() error {
		current, err := client.GetBox(ctx, box.ID)
		if err != nil {
			return err
		}
		if err := validateBoxIdentity(current, box); err != nil {
			return err
		}
		if err := client.PrepareSSH(ctx, box.ID); err != nil {
			return err
		}
		lease, err = b.leaseFromBox(ctx, cfg, current, leaseID, slug, req.Keep, true)
		return err
	})
	if err != nil {
		if !req.Keep {
			err = errors.Join(err, b.rollbackBox(ctx, client, leaseID, box, claim, true))
		}
		return core.LeaseTarget{}, fmt.Errorf("ascii-box lease=%s box=%s preparation failed: %w", leaseID, box.ID, err)
	}
	core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
	if req.OnAcquired != nil {
		if err := req.OnAcquired(lease); err != nil {
			if !req.Keep {
				err = errors.Join(err, b.rollbackBox(ctx, client, leaseID, box, claim, true))
			}
			return core.LeaseTarget{}, err
		}
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s slug=%s box=%s host=%s state=%s\n", leaseID, slug, box.ID, boxHost(box), boxState(box))
	return lease, nil
}

func (b *backend) rollbackBox(ctx context.Context, client api, leaseID string, box boxData, claim core.LeaseClaim, exists bool) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), boxReleaseTimeout)
	defer cancel()
	if exists {
		if claim.LeaseID != leaseID || box.ID != box.createdID || !concreteBoxID(box.createdID) {
			return core.Exit(2, "ascii-box rollback has no matching original publication identity")
		}
		if err := validateBoxIdentity(box, boxFromClaim(claim)); err != nil {
			return err
		}
		return releaseClaimedBox(cleanupCtx, client, claim, nil)
	}
	return core.CleanupLeaseClaimIfUnchangedAfter(leaseID, claim, false, func() error {
		if box.ID != box.createdID || !concreteBoxID(box.createdID) {
			return core.Exit(2, "ascii-box rollback has no original creation identity")
		}
		if boxCreationTime(box) == "" {
			fresh, err := client.GetBox(cleanupCtx, box.ID)
			if err != nil {
				return err
			}
			if fresh.ID != box.ID || boxCreationTime(fresh) == "" {
				return core.Exit(2, "ascii-box rollback cannot establish creation identity for %s; retained", box.ID)
			}
			box = mergeBox(box, fresh)
		}
		return releaseExactBox(cleanupCtx, client, box, nil, nil)
	})
}

func (b *backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	cfg, err := b.configForRun()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	client, err := newAPI(cfg, b.rt)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.IsReadOnlyStatus() {
		leaseID, boxID, slug, err := b.resolveBoxID(ctx, client, req.ID)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		box, err := client.GetBox(ctx, boxID)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		return core.LeaseTarget{Server: boxToServer(cfg, box, leaseID, slug, true), LeaseID: leaseID}, nil
	}
	claim, err := resolveOwnedBox(cfg, req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	var lease core.LeaseTarget
	err = core.WithLeaseClaimUnchanged(claim.LeaseID, claim, func() error {
		if !req.ReleaseOnly && req.Repo.Root != "" {
			if err := core.CheckLeaseClaimRepositoryOwner(claim.LeaseID, claim, req.Repo.Root, req.Reclaim); err != nil {
				return err
			}
		}
		var box boxData
		if req.ReleaseOnly {
			var absent bool
			box, absent, err = exactBoxForRelease(ctx, client, boxFromClaim(claim))
			if err != nil {
				return err
			}
			if absent {
				box = boxFromClaim(claim)
			}
		} else {
			box, err = client.GetBox(ctx, claim.CloudID)
			if err != nil {
				return err
			}
		}
		if err := validateBoxIdentity(box, boxFromClaim(claim)); err != nil {
			return err
		}
		lease = core.LeaseTarget{Server: boxToServer(cfg, box, claim.LeaseID, claim.Slug, true), LeaseID: claim.LeaseID}
		if req.ReleaseOnly {
			return nil
		}
		if err := client.PrepareSSH(ctx, box.ID); err != nil {
			return err
		}
		lease, err = b.leaseFromBox(ctx, cfg, box, claim.LeaseID, claim.Slug, true, true)
		return err
	})
	if err != nil {
		return core.LeaseTarget{}, err
	}
	core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
	return lease, nil
}

func (b *backend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	_ = req
	cfg, err := b.configForRun()
	if err != nil {
		return nil, err
	}
	client, err := newAPI(cfg, b.rt)
	if err != nil {
		return nil, err
	}
	boxes, err := client.ListBoxes(ctx, false)
	if err != nil {
		return nil, err
	}
	claims, err := boxClaimsByID(cfg)
	if err != nil {
		return nil, err
	}
	out := make([]core.Server, 0, len(boxes))
	for _, box := range boxes {
		leaseID, slug, ok := b.boxLeaseMetadata(box, claims)
		if !ok {
			continue
		}
		out = append(out, boxToServer(cfg, box, leaseID, slug, true))
	}
	return out, nil
}

func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	cfg, err := b.configForRun()
	if err != nil {
		return core.DoctorResult{}, err
	}
	client, err := newAPI(cfg, b.rt)
	if err != nil {
		return core.DoctorResult{}, err
	}
	if err := client.Check(ctx); err != nil {
		return core.DoctorResult{}, err
	}
	return core.DoctorResult{
		Provider: providerName,
		Message:  "auth=ready cli=ready control_plane=ready limits=ready mutation=false runtime=unchecked",
	}, nil
}

func (b *backend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	cfg, err := b.configForRun()
	if err != nil {
		return core.StatusView{}, err
	}
	client, err := newAPI(cfg, b.rt)
	if err != nil {
		return core.StatusView{}, err
	}
	leaseID, boxID, slug, err := b.resolveBoxID(ctx, client, req.ID)
	if err != nil {
		return core.StatusView{}, err
	}
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(5 * time.Minute)
	}
	for {
		box, err := client.GetBox(ctx, boxID)
		if err != nil {
			return core.StatusView{}, err
		}
		view := statusFromBox(cfg, box, leaseID, slug)
		if !req.Wait || view.Ready {
			return view, nil
		}
		if boxStateFailed(view.State) {
			return view, nil
		}
		if b.now().After(deadline) {
			return core.StatusView{}, core.Exit(5, "timed out waiting for ascii-box %s to become ready", boxID)
		}
		select {
		case <-ctx.Done():
			return core.StatusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	cfg, err := b.configForRun()
	if err != nil {
		return err
	}
	client, err := newAPI(cfg, b.rt)
	if err != nil {
		return err
	}
	if err := core.ValidateLeaseTargetProviderIdentity(req.Lease, req.ExpectedProviderIdentity); err != nil {
		return err
	}
	claim, err := shared.RequireClaimSnapshot(req.Lease.Server, providerName)
	if err != nil {
		return err
	}
	if _, err := boxClaimBinding(cfg, claim); err != nil {
		return err
	}
	if req.Lease.LeaseID != claim.LeaseID || req.Lease.Server.CloudID != claim.CloudID || req.Lease.Server.Labels["box_id"] != claim.CloudID {
		return core.Exit(2, "ascii-box release target differs from its original claim")
	}
	ctx, cancel := context.WithTimeout(ctx, boxReleaseTimeout)
	defer cancel()
	return releaseClaimedBox(ctx, client, claim, func(box boxData) {
		if req.GuardedRemoteCleanup != nil {
			lease := req.Lease
			lease.Server = boxToServer(cfg, box, claim.LeaseID, claim.Slug, true)
			if target, err := boxSSHTarget(cfg, box); err == nil {
				lease.SSH = target
				req.GuardedRemoteCleanup(ctx, lease)
			}
		}
	})
}

func (b *backend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	return fmt.Sprintf("released lease=%s box=%s", lease.LeaseID, core.Blank(lease.Server.CloudID, lease.Server.Labels["box_id"]))
}

func (b *backend) Touch(_ context.Context, req core.TouchRequest) (core.Server, error) {
	cfg, err := b.configForRun()
	if err != nil {
		return core.Server{}, err
	}
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, cfg, req.State, time.Now().UTC())
	server.Status = req.State
	return server, nil
}

func (b *backend) leaseFromBox(ctx context.Context, cfg core.Config, box boxData, leaseID, slug string, keep, waitSSH bool) (core.LeaseTarget, error) {
	server := boxToServer(cfg, box, leaseID, slug, keep)
	target, err := boxSSHTarget(cfg, box)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if waitSSH {
		if err := waitForSSHReadyFunc(ctx, &target, b.rt.Stderr, "ascii-box ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
			return core.LeaseTarget{}, err
		}
		server.Labels["state"] = "ready"
		server.Status = "ready"
	}
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

// resolveBoxID is discovery only. It must never create or update ownership.
func (b *backend) resolveBoxID(ctx context.Context, client api, id string) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", core.Exit(2, "provider=%s requires a Crabbox lease id, slug, or ASCII Box id", providerName)
	}
	if claim, ok, err := core.ResolveLeaseClaimForProvider(id, providerName); err != nil {
		return "", "", "", err
	} else if ok {
		boxID := core.Blank(claim.CloudID, boxIDFromScope(claim.ProviderScope))
		if boxID == "" {
			box, err := resolveLegacyBoxByLease(ctx, client, claim.LeaseID)
			if err != nil {
				return "", "", "", err
			}
			boxID = box.ID
		}
		return claim.LeaseID, boxID, claim.Slug, nil
	}
	if box, err := client.GetBox(ctx, id); err == nil {
		leaseID := boxLeaseID(box)
		slug := boxSlug(leaseID, box)
		return leaseID, box.ID, slug, nil
	} else if err != nil && !isNotFound(err) {
		return "", "", "", err
	}
	if strings.HasPrefix(id, "cbx_") {
		box, err := resolveLegacyBoxByLease(ctx, client, id)
		if err != nil {
			return "", "", "", err
		}
		return id, box.ID, boxSlug(id, box), nil
	}
	box, err := resolveLegacyBoxBySlug(ctx, client, id)
	if err != nil {
		return "", "", "", err
	}
	leaseID := boxLeaseID(box)
	return leaseID, box.ID, boxSlug(leaseID, box), nil
}

func resolveLegacyBoxByLease(ctx context.Context, client api, leaseID string) (boxData, error) {
	boxes, err := client.ListBoxes(ctx, false)
	if err != nil {
		return boxData{}, err
	}
	for _, box := range boxes {
		if isCrabboxBox(box) && boxLeaseID(box) == leaseID {
			return box, nil
		}
	}
	return boxData{}, core.Exit(4, "ascii-box lease %q was not found", leaseID)
}

func resolveLegacyBoxBySlug(ctx context.Context, client api, slug string) (boxData, error) {
	boxes, err := client.ListBoxes(ctx, false)
	if err != nil {
		return boxData{}, err
	}
	for _, box := range boxes {
		if !isCrabboxBox(box) {
			continue
		}
		leaseID := boxLeaseID(box)
		if boxSlug(leaseID, box) == slug {
			return box, nil
		}
	}
	return boxData{}, core.Exit(4, "ascii-box %q was not found", slug)
}

func (b *backend) boxLeaseMetadata(box boxData, claims map[string]core.LeaseClaim) (string, string, bool) {
	if claim, ok := claims[box.ID]; ok {
		return claim.LeaseID, claim.Slug, true
	}
	if isCrabboxBox(box) {
		leaseID := boxLeaseID(box)
		return leaseID, boxSlug(leaseID, box), true
	}
	return "", "", false
}

func (b *backend) configForRun() (core.Config, error) {
	cfg := b.cfg
	cfg.Provider = providerName
	cfg.TargetOS = targetLinux
	cfg.Network = networkPublic
	cfg.SSHPort = core.Blank(strings.TrimSpace(cfg.SSHPort), "22")
	cleaned, err := cleanWorkdir(workdir(cfg))
	if err != nil {
		return core.Config{}, err
	}
	cfg.WorkRoot = cleaned
	return cfg, nil
}

func (b *backend) now() time.Time {
	return now(b.rt)
}

func boxToServer(cfg core.Config, box boxData, leaseID, slug string, keep bool) core.Server {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, time.Now().UTC())
	labels["box_id"] = box.ID
	labels["ascii_box_scope"] = (Provider{}).ClaimScope(cfg)
	labels[boxCreationLabel] = boxCreationTime(box)
	labels["box_name"] = box.Name
	labels["box_state"] = boxState(box)
	labels["ssh_user"] = boxSSHUser(box)
	labels["work_root"] = cfg.WorkRoot
	if expiresAt := boxExpiresAt(box); expiresAt != "" {
		labels["expires_at"] = expiresAt
	}
	server := core.Server{
		Provider: providerName,
		CloudID:  box.ID,
		Name:     core.Blank(box.Name, core.LeaseProviderName(leaseID, slug)),
		Status:   boxState(box),
		Labels:   labels,
	}
	server.ServerType.Name = "ascii-box"
	server.PublicNet.IPv4.IP = boxHost(box)
	return server
}

func statusFromBox(cfg core.Config, box boxData, leaseID, slug string) core.StatusView {
	server := boxToServer(cfg, box, leaseID, slug, true)
	host := boxHost(box)
	sshHost := host
	port := "22"
	if endpointHost, endpointPort, err := boxSSHConnection(box); err == nil {
		sshHost = endpointHost
		port = endpointPort
	}
	user := boxSSHUser(box)
	return core.StatusView{
		ID:         leaseID,
		Slug:       slug,
		Provider:   providerName,
		TargetOS:   targetLinux,
		State:      boxState(box),
		ServerID:   box.ID,
		ServerType: server.ServerType.Name,
		Host:       host,
		Network:    networkPublic,
		SSHHost:    sshHost,
		SSHUser:    user,
		SSHPort:    port,
		SSHKey:     boxSSHKey(cfg),
		ExpiresAt:  boxExpiresAt(box),
		Labels:     server.Labels,
		HasHost:    host != "",
		Ready:      boxReadyForSSH(box),
	}
}

func boxSSHTarget(cfg core.Config, box boxData) (core.SSHTarget, error) {
	host, port, err := boxSSHConnection(box)
	if err != nil {
		return core.SSHTarget{}, err
	}
	user := boxSSHUser(box)
	if user == "" {
		return core.SSHTarget{}, core.Exit(5, "ascii-box %s is missing SSH user", box.ID)
	}
	return core.SSHTarget{
		User:            user,
		Host:            host,
		Key:             boxSSHKey(cfg),
		Port:            port,
		TargetOS:        targetLinux,
		NetworkKind:     networkPublic,
		NoControlMaster: true,
		ReadyCheck:      "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null && command -v python3 >/dev/null",
	}, nil
}

func boxSSHConnection(box boxData) (string, string, error) {
	if endpoint := strings.TrimSpace(shared.FirstNonBlankTrimmed(box.SSHEndpoint, box.SSHEndpointAlt)); endpoint != "" {
		host, port, err := net.SplitHostPort(endpoint)
		if err != nil || strings.TrimSpace(host) == "" || strings.TrimSpace(port) == "" {
			return "", "", core.Exit(5, "ascii-box %s has invalid SSH endpoint %q", box.ID, endpoint)
		}
		return strings.TrimSpace(host), strings.TrimSpace(port), nil
	}
	host := boxHost(box)
	if host == "" {
		return "", "", core.Exit(5, "ascii-box %s is missing ip for SSH", box.ID)
	}
	return host, "22", nil
}

func boxSSHKey(cfg core.Config) string {
	return path.Join(asciiBoxCLIHome(), ".ssh", "ascii_box_ed25519")
}

func boxHost(box boxData) string {
	return shared.FirstNonBlankTrimmed(box.IP, box.MachineIP, box.MachineIPAlt, box.PublicIP)
}

func boxSSHUser(box boxData) string {
	return shared.FirstNonBlankTrimmed(box.SSHUser, box.SSHUserAlt, "user")
}

func boxState(box boxData) string {
	return strings.ToLower(core.Blank(shared.FirstNonBlankTrimmed(box.Status, box.State), "provisioning"))
}

func boxExpiresAt(box boxData) string {
	switch value := box.ExpiresAt.(type) {
	case string:
		return strings.TrimSpace(value)
	case float64:
		return fmt.Sprintf("%.0f", value)
	case json.Number:
		return value.String()
	default:
		switch value := box.ArchiveAfter.(type) {
		case string:
			return strings.TrimSpace(value)
		case float64:
			return fmt.Sprintf("%.0f", value)
		case json.Number:
			return value.String()
		default:
			return ""
		}
	}
}

func boxReadyForSSH(box boxData) bool {
	return statusReady(boxState(box)) && boxHost(box) != "" && boxSSHUser(box) != ""
}

func boxStateFailed(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "error", "failed", "failure", "stopped", "terminated", "deleted":
		return true
	default:
		return false
	}
}

func statusReady(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running", "ready", "idle", "paused", "active":
		return true
	default:
		return false
	}
}

var boxNamePattern = regexp.MustCompile(`^crabbox-(.+)-([0-9a-f]{12})$`)

func isCrabboxBox(box boxData) bool {
	return boxNamePattern.MatchString(strings.TrimSpace(box.Name))
}

func boxLeaseID(box boxData) string {
	if match := boxNamePattern.FindStringSubmatch(strings.TrimSpace(box.Name)); len(match) == 3 {
		return "cbx_" + match[2]
	}
	return "ascii_" + box.ID
}

func boxSlug(leaseID string, box boxData) string {
	if match := boxNamePattern.FindStringSubmatch(strings.TrimSpace(box.Name)); len(match) == 3 {
		return match[1]
	}
	return core.NewLeaseSlug(leaseID)
}

func boxIDFromScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if !strings.HasPrefix(scope, "box:") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(scope, "box:"))
}

func boxClaimsByID(cfg core.Config) (map[string]core.LeaseClaim, error) {
	return shared.IndexProviderClaims(providerName, func(claim core.LeaseClaim) string {
		boxID := boxIDFromScope(claim.ProviderScope)
		if claim.ProviderScope == (Provider{}).ClaimScope(cfg) {
			boxID = claim.CloudID
		}
		return boxID
	})
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var identityErr *boxIdentityError
	if errors.As(err, &identityErr) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "404") || strings.Contains(msg, "not found")
}

func workdir(cfg core.Config) string {
	return core.Blank(strings.TrimSpace(cfg.AsciiBox.Workdir), "/home/user/crabbox")
}

func cleanWorkdir(workdir string) (string, error) {
	return shared.CleanPOSIXWorkspacePath("ascii-box workdir", workdir, "/home/user", "/workspace", "/workspace/home")
}

var waitForSSHReadyFunc = core.WaitForSSHReady

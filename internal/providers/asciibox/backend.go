package asciibox

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func NewBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	cfg.TargetOS = targetLinux
	cfg.Network = networkPublic
	if cleaned, err := cleanWorkdir(workdir(cfg)); err == nil {
		cfg.WorkRoot = cleaned
	}
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	cfg, err := b.configForRun()
	if err != nil {
		return LeaseTarget{}, err
	}
	client, err := newAPI(cfg, b.rt)
	if err != nil {
		return LeaseTarget{}, err
	}
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, req.RequestedSlug)
	if err != nil {
		return LeaseTarget{}, err
	}
	ttl := req.Options.TTL
	if ttl <= 0 {
		ttl = cfg.TTL
	}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s ttl=%s\n", providerName, leaseID, slug, blank(ttl.String(), "-"))
	box, err := client.CreateBox(ctx, createRequest{TTL: ttl})
	if err != nil {
		if box.ID != "" {
			_ = client.ReleaseBox(context.Background(), box.ID)
		}
		return LeaseTarget{}, err
	}
	if err := claimLeaseForRepoProviderScope(leaseID, slug, providerName, boxScope(box.ID), req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		_ = client.ReleaseBox(context.Background(), box.ID)
		return LeaseTarget{}, err
	}
	if err := client.PrepareSSH(ctx, box.ID); err != nil {
		if !req.Keep {
			_ = client.ReleaseBox(context.Background(), box.ID)
			removeLeaseClaim(leaseID)
		}
		return LeaseTarget{}, err
	}
	lease, err := b.leaseFromBox(ctx, cfg, box, leaseID, slug, req.Keep, true)
	if err != nil {
		if !req.Keep {
			_ = client.ReleaseBox(context.Background(), box.ID)
			removeLeaseClaim(leaseID)
		}
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s slug=%s box=%s host=%s state=%s\n", leaseID, slug, box.ID, boxHost(box), boxState(box))
	return lease, nil
}

func (b *backend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	cfg, err := b.configForRun()
	if err != nil {
		return LeaseTarget{}, err
	}
	client, err := newAPI(cfg, b.rt)
	if err != nil {
		return LeaseTarget{}, err
	}
	leaseID, boxID, slug, err := b.resolveBoxID(ctx, client, req.ID, req.Repo.Root, cfg.IdleTimeout, req.Reclaim)
	if err != nil {
		return LeaseTarget{}, err
	}
	box, err := client.GetBox(ctx, boxID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		return LeaseTarget{Server: boxToServer(cfg, box, leaseID, slug, true), LeaseID: leaseID}, nil
	}
	if err := client.PrepareSSH(ctx, box.ID); err != nil {
		return LeaseTarget{}, err
	}
	return b.leaseFromBox(ctx, cfg, box, leaseID, slug, true, true)
}

func (b *backend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	cfg, err := b.configForRun()
	if err != nil {
		return nil, err
	}
	client, err := newAPI(cfg, b.rt)
	if err != nil {
		return nil, err
	}
	boxes, err := client.ListBoxes(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := boxClaimsByID()
	if err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(boxes))
	for _, box := range boxes {
		leaseID, slug, ok := b.boxLeaseMetadata(box, claims)
		if !ok {
			continue
		}
		out = append(out, boxToServer(cfg, box, leaseID, slug, true))
	}
	return out, nil
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	cfg, err := b.configForRun()
	if err != nil {
		return DoctorResult{}, err
	}
	client, err := newAPI(cfg, b.rt)
	if err != nil {
		return DoctorResult{}, err
	}
	if err := client.Check(ctx); err != nil {
		return DoctorResult{}, err
	}
	return DoctorResult{
		Provider: providerName,
		Message:  "auth=ready cli=ready control_plane=ready limits=ready mutation=false runtime=unchecked",
	}, nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	cfg, err := b.configForRun()
	if err != nil {
		return StatusView{}, err
	}
	client, err := newAPI(cfg, b.rt)
	if err != nil {
		return StatusView{}, err
	}
	leaseID, boxID, slug, err := b.resolveBoxID(ctx, client, req.ID, "", 0, false)
	if err != nil {
		return StatusView{}, err
	}
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(5 * time.Minute)
	}
	for {
		box, err := client.GetBox(ctx, boxID)
		if err != nil {
			return StatusView{}, err
		}
		view := statusFromBox(cfg, box, leaseID, slug)
		if !req.Wait || view.Ready {
			return view, nil
		}
		if boxStateFailed(view.State) {
			return view, nil
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for ascii-box %s to become ready", boxID)
		}
		select {
		case <-ctx.Done():
			return StatusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *backend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	cfg, err := b.configForRun()
	if err != nil {
		return err
	}
	client, err := newAPI(cfg, b.rt)
	if err != nil {
		return err
	}
	boxID := strings.TrimSpace(req.Lease.Server.CloudID)
	if boxID == "" {
		boxID = strings.TrimSpace(req.Lease.Server.Labels["box_id"])
	}
	if boxID == "" && req.Lease.LeaseID != "" {
		_, resolvedBoxID, _, err := b.resolveBoxID(ctx, client, req.Lease.LeaseID, "", 0, false)
		if err != nil {
			return err
		}
		boxID = resolvedBoxID
	}
	if boxID == "" {
		return exit(2, "provider=%s requires an ASCII Box id to release", providerName)
	}
	if err := client.ReleaseBox(ctx, boxID); err != nil {
		return err
	}
	removeLeaseClaim(req.Lease.LeaseID)
	return nil
}

func (b *backend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("released lease=%s box=%s", lease.LeaseID, blank(lease.Server.CloudID, lease.Server.Labels["box_id"]))
}

func (b *backend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	cfg, err := b.configForRun()
	if err != nil {
		return Server{}, err
	}
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = touchDirectLeaseLabels(server.Labels, cfg, req.State, time.Now().UTC())
	server.Status = req.State
	return server, nil
}

func (b *backend) leaseFromBox(ctx context.Context, cfg Config, box boxData, leaseID, slug string, keep, waitSSH bool) (LeaseTarget, error) {
	server := boxToServer(cfg, box, leaseID, slug, keep)
	target, err := boxSSHTarget(cfg, box)
	if err != nil {
		return LeaseTarget{}, err
	}
	if waitSSH {
		if err := waitForSSHReadyFunc(ctx, &target, b.rt.Stderr, "ascii-box ssh", bootstrapWaitTimeout(cfg)); err != nil {
			return LeaseTarget{}, err
		}
		server.Labels["state"] = "ready"
		server.Status = "ready"
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *backend) resolveBoxID(ctx context.Context, client api, id, repoRoot string, idleTimeout time.Duration, reclaim bool) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", exit(2, "provider=%s requires a Crabbox lease id, slug, or ASCII Box id", providerName)
	}
	if claim, ok, err := resolveLeaseClaimForProvider(id, providerName); err != nil {
		return "", "", "", err
	} else if ok {
		boxID := boxIDFromScope(claim.ProviderScope)
		if boxID == "" {
			box, err := resolveLegacyBoxByLease(ctx, client, claim.LeaseID)
			if err != nil {
				return "", "", "", err
			}
			boxID = box.ID
		}
		if repoRoot != "" {
			if err := claimLeaseForRepoProviderScope(claim.LeaseID, claim.Slug, providerName, boxScope(boxID), repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim); err != nil {
				return "", "", "", err
			}
		}
		return claim.LeaseID, boxID, claim.Slug, nil
	}
	if box, err := client.GetBox(ctx, id); err == nil {
		leaseID := boxLeaseID(box)
		slug := boxSlug(leaseID, box)
		if repoRoot != "" {
			if err := claimLeaseForRepoProviderScope(leaseID, slug, providerName, boxScope(box.ID), repoRoot, idleTimeout, reclaim); err != nil {
				return "", "", "", err
			}
		}
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
	boxes, err := client.ListBoxes(ctx)
	if err != nil {
		return boxData{}, err
	}
	for _, box := range boxes {
		if isCrabboxBox(box) && boxLeaseID(box) == leaseID {
			return box, nil
		}
	}
	return boxData{}, exit(4, "ascii-box lease %q was not found", leaseID)
}

func resolveLegacyBoxBySlug(ctx context.Context, client api, slug string) (boxData, error) {
	boxes, err := client.ListBoxes(ctx)
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
	return boxData{}, exit(4, "ascii-box %q was not found", slug)
}

func (b *backend) boxLeaseMetadata(box boxData, claims map[string]LeaseClaim) (string, string, bool) {
	if claim, ok := claims[box.ID]; ok {
		return claim.LeaseID, claim.Slug, true
	}
	if isCrabboxBox(box) {
		leaseID := boxLeaseID(box)
		return leaseID, boxSlug(leaseID, box), true
	}
	return "", "", false
}

func (b *backend) configForRun() (Config, error) {
	cfg := b.cfg
	cfg.Provider = providerName
	cfg.TargetOS = targetLinux
	cfg.Network = networkPublic
	cfg.SSHPort = blank(strings.TrimSpace(cfg.SSHPort), "22")
	cleaned, err := cleanWorkdir(workdir(cfg))
	if err != nil {
		return Config{}, err
	}
	cfg.WorkRoot = cleaned
	return cfg, nil
}

func (b *backend) now() time.Time {
	return now(b.rt)
}

func boxToServer(cfg Config, box boxData, leaseID, slug string, keep bool) Server {
	labels := directLeaseLabels(cfg, leaseID, slug, providerName, "", keep, time.Now().UTC())
	labels["box_id"] = box.ID
	labels["box_name"] = box.Name
	labels["box_state"] = boxState(box)
	labels["ssh_user"] = boxSSHUser(box)
	labels["work_root"] = cfg.WorkRoot
	if expiresAt := boxExpiresAt(box); expiresAt != "" {
		labels["expires_at"] = expiresAt
	}
	server := Server{
		Provider: providerName,
		CloudID:  box.ID,
		Name:     blank(box.Name, leaseProviderName(leaseID, slug)),
		Status:   boxState(box),
		Labels:   labels,
	}
	server.ServerType.Name = "ascii-box"
	server.PublicNet.IPv4.IP = boxHost(box)
	return server
}

func statusFromBox(cfg Config, box boxData, leaseID, slug string) StatusView {
	server := boxToServer(cfg, box, leaseID, slug, true)
	host := boxHost(box)
	user := boxSSHUser(box)
	return StatusView{
		ID:         leaseID,
		Slug:       slug,
		Provider:   providerName,
		TargetOS:   targetLinux,
		State:      boxState(box),
		ServerID:   box.ID,
		ServerType: server.ServerType.Name,
		Host:       host,
		Network:    networkPublic,
		SSHHost:    host,
		SSHUser:    user,
		SSHPort:    "22",
		SSHKey:     boxSSHKey(cfg),
		ExpiresAt:  boxExpiresAt(box),
		Labels:     server.Labels,
		HasHost:    host != "",
		Ready:      boxReadyForSSH(box),
	}
}

func boxSSHTarget(cfg Config, box boxData) (SSHTarget, error) {
	host := boxHost(box)
	if host == "" {
		return SSHTarget{}, exit(5, "ascii-box %s is missing ip for SSH", box.ID)
	}
	user := boxSSHUser(box)
	if user == "" {
		return SSHTarget{}, exit(5, "ascii-box %s is missing SSH user", box.ID)
	}
	return SSHTarget{
		User:            user,
		Host:            host,
		Key:             boxSSHKey(cfg),
		Port:            "22",
		TargetOS:        targetLinux,
		NetworkKind:     networkPublic,
		NoControlMaster: true,
		ReadyCheck:      "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null && command -v python3 >/dev/null",
	}, nil
}

func boxSSHKey(cfg Config) string {
	return path.Join(asciiBoxCLIHome(), ".ssh", "ascii_box_ed25519")
}

func boxHost(box boxData) string {
	return firstNonBlank(box.IP, box.MachineIP, box.MachineIPAlt, box.PublicIP)
}

func boxSSHUser(box boxData) string {
	return firstNonBlank(box.SSHUser, box.SSHUserAlt, "user")
}

func boxState(box boxData) string {
	return strings.ToLower(blank(firstNonBlank(box.Status, box.State), "provisioning"))
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
	return newLeaseSlug(leaseID)
}

func boxScope(boxID string) string {
	if strings.TrimSpace(boxID) == "" {
		return ""
	}
	return "box:" + strings.TrimSpace(boxID)
}

func boxIDFromScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if !strings.HasPrefix(scope, "box:") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(scope, "box:"))
}

func boxClaimsByID() (map[string]LeaseClaim, error) {
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	out := map[string]LeaseClaim{}
	for _, claim := range claims {
		if claim.Provider != providerName {
			continue
		}
		boxID := boxIDFromScope(claim.ProviderScope)
		if boxID == "" {
			continue
		}
		out[boxID] = claim
	}
	return out, nil
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "404") || strings.Contains(msg, "not found")
}

func workdir(cfg Config) string {
	return blank(strings.TrimSpace(cfg.AsciiBox.Workdir), "/home/user/crabbox")
}

func cleanWorkdir(workdir string) (string, error) {
	trimmed := strings.TrimSpace(workdir)
	if trimmed == "" {
		return "", exit(2, "ascii-box workdir is empty")
	}
	clean := path.Clean(trimmed)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "ascii-box workdir %q must resolve to an absolute path", workdir)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/home/user", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace", "/workspace/home":
		return "", exit(2, "ascii-box workdir %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

var waitForSSHReadyFunc = waitForSSHReady

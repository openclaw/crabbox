package daytona

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	daytona "github.com/daytonaio/daytona/libs/api-client-go"
)

const (
	daytonaProvider      = "daytona"
	daytonaTokenRedacted = "<token>"
)

type daytonaFlagValues struct {
	APIURL           *string
	Snapshot         *string
	Target           *string
	User             *string
	WorkRoot         *string
	SSHGatewayHost   *string
	SSHAccessMinutes *int
}

func RegisterDaytonaProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return daytonaFlagValues{
		APIURL:           fs.String("daytona-api-url", defaults.Daytona.APIURL, "Daytona API URL"),
		Snapshot:         fs.String("daytona-snapshot", defaults.Daytona.Snapshot, "Daytona snapshot name"),
		Target:           fs.String("daytona-target", defaults.Daytona.Target, "Daytona compute target"),
		User:             fs.String("daytona-user", defaults.Daytona.User, "Daytona sandbox user"),
		WorkRoot:         fs.String("daytona-work-root", defaults.Daytona.WorkRoot, "Daytona sandbox work root"),
		SSHGatewayHost:   fs.String("daytona-ssh-gateway-host", defaults.Daytona.SSHGatewayHost, "Daytona SSH gateway host"),
		SSHAccessMinutes: fs.Int("daytona-ssh-access-minutes", defaults.Daytona.SSHAccessMinutes, "Daytona SSH access token TTL in minutes"),
	}
}

func ApplyDaytonaProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == daytonaProvider {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=daytona; choose CPU, memory, and disk in the Daytona snapshot")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=daytona; choose CPU, memory, and disk in the Daytona snapshot")
		}
	}
	v, ok := values.(daytonaFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "daytona-api-url") {
		cfg.Daytona.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "daytona-snapshot") {
		cfg.Daytona.Snapshot = *v.Snapshot
	}
	if flagWasSet(fs, "daytona-target") {
		cfg.Daytona.Target = *v.Target
	}
	if flagWasSet(fs, "daytona-user") {
		cfg.Daytona.User = *v.User
	}
	if flagWasSet(fs, "daytona-work-root") {
		cfg.Daytona.WorkRoot = *v.WorkRoot
	}
	if flagWasSet(fs, "daytona-ssh-gateway-host") {
		cfg.Daytona.SSHGatewayHost = *v.SSHGatewayHost
	}
	if flagWasSet(fs, "daytona-ssh-access-minutes") {
		cfg.Daytona.SSHAccessMinutes = *v.SSHAccessMinutes
	}
	return nil
}

func NewDaytonaLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = daytonaProvider
	return &daytonaLeaseBackend{spec: spec, cfg: cfg, rt: rt}
}

type daytonaLeaseBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *daytonaLeaseBackend) Spec() ProviderSpec { return b.spec }

func (b *daytonaLeaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	if strings.TrimSpace(b.cfg.Daytona.Snapshot) == "" {
		return LeaseTarget{}, exit(2, "provider=daytona requires --daytona-snapshot or daytona.snapshot")
	}
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return LeaseTarget{}, err
	}
	leaseID := newLeaseID()
	existing, err := client.ListCrabboxSandboxes(ctx)
	if err != nil {
		return LeaseTarget{}, daytonaError("list sandboxes", err)
	}
	slug := allocateDirectLeaseSlug(leaseID, daytonaSandboxesToServers(existing, b.cfg))
	cfg := b.cfg
	cfg.ServerType = "snapshot"
	cfg.WorkRoot = daytonaWorkRoot(cfg)
	cfg.SSHKey = ""
	cfg.SSHUser = daytonaUser(cfg)
	cfg.SSHPort = "22"
	now := time.Now().UTC()
	labels := directLeaseLabels(cfg, leaseID, slug, daytonaProvider, "", req.Keep, now)
	labels["lease_name"] = leaseProviderName(leaseID, slug)
	labels["work_root"] = cfg.WorkRoot
	create := daytona.NewCreateSandbox()
	create.SetName(labels["lease_name"])
	create.SetSnapshot(strings.TrimSpace(cfg.Daytona.Snapshot))
	create.SetLabels(labels)
	if target := strings.TrimSpace(cfg.Daytona.Target); target != "" {
		create.SetTarget(target)
	}
	if user := daytonaUser(cfg); user != "" {
		create.SetUser(user)
	}
	autoStop := int32(durationMinutesCeil(cfg.IdleTimeout))
	create.SetAutoStopInterval(autoStop)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=daytona lease=%s slug=%s snapshot=%s target=%s keep=%v\n", leaseID, slug, cfg.Daytona.Snapshot, blank(cfg.Daytona.Target, "-"), req.Keep)
	created, err := client.CreateSandbox(ctx, *create)
	if err != nil {
		return LeaseTarget{}, daytonaError("create sandbox", err)
	}
	sandbox, err := waitForDaytonaReady(ctx, client, created.GetId(), 5*time.Minute)
	if err != nil {
		if !req.Keep {
			_ = client.DeleteSandbox(context.Background(), created.GetId())
		}
		return LeaseTarget{}, err
	}
	server := daytonaSandboxToServer(sandbox, cfg)
	server.Labels["state"] = "ready"
	if err := client.ReplaceLabels(ctx, server.CloudID, server.Labels); err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: set labels: %v\n", daytonaError("replace labels", err))
	}
	target, err := daytonaSSHTargetFor(ctx, client, cfg, server)
	if err != nil {
		if !req.Keep {
			_ = client.DeleteSandbox(context.Background(), server.CloudID)
		}
		return LeaseTarget{}, err
	}
	if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "daytona ssh", bootstrapWaitTimeout(cfg)); err != nil {
		if !req.Keep {
			_ = client.DeleteSandbox(context.Background(), server.CloudID)
		}
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s sandbox=%s state=%s\n", leaseID, server.CloudID, server.Status)
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *daytonaLeaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return LeaseTarget{}, err
	}
	sandbox, leaseID, err := resolveDaytonaSandbox(ctx, client, b.cfg, req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if !daytonaStateReady(daytonaSandboxState(sandbox)) {
		if daytonaStateFailed(daytonaSandboxState(sandbox)) {
			return LeaseTarget{}, exit(5, "daytona sandbox %s entered terminal state=%s", sandbox.GetId(), daytonaSandboxState(sandbox))
		}
		sandbox, err = client.StartSandbox(ctx, sandbox.GetId())
		if err != nil {
			return LeaseTarget{}, daytonaError("start sandbox", err)
		}
		sandbox, err = waitForDaytonaReady(ctx, client, sandbox.GetId(), 5*time.Minute)
		if err != nil {
			return LeaseTarget{}, err
		}
	}
	server := daytonaSandboxToServer(sandbox, b.cfg)
	target, err := daytonaSSHTargetFor(ctx, client, b.cfg, server)
	if err != nil {
		return LeaseTarget{}, err
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *daytonaLeaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	sandboxes, err := client.ListCrabboxSandboxes(ctx)
	if err != nil {
		return nil, daytonaError("list sandboxes", err)
	}
	return daytonaSandboxesToServers(sandboxes, b.cfg), nil
}

func (b *daytonaLeaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	if req.Lease.Server.CloudID != "" {
		if err := client.DeleteSandbox(ctx, req.Lease.Server.CloudID); err != nil {
			return daytonaError("delete sandbox", err)
		}
	}
	removeLeaseClaim(req.Lease.LeaseID)
	return nil
}

func (b *daytonaLeaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return req.Lease.Server, err
	}
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = touchDirectLeaseLabels(server.Labels, b.cfg, req.State, time.Now().UTC())
	if server.CloudID != "" {
		if err := client.ReplaceLabels(ctx, server.CloudID, server.Labels); err != nil {
			return server, daytonaError("replace labels", err)
		}
		if err := client.UpdateLastActivity(ctx, server.CloudID); err != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: daytona last-activity: %v\n", daytonaError("update last activity", err))
		}
	}
	return server, nil
}

func waitForDaytonaReady(ctx context.Context, client daytonaAPI, id string, timeout time.Duration) (*daytona.Sandbox, error) {
	deadline := time.Now().Add(timeout)
	for {
		sandbox, err := client.GetSandbox(ctx, id)
		if err != nil {
			return nil, daytonaError("get sandbox", err)
		}
		state := daytonaSandboxState(sandbox)
		if daytonaStateReady(state) {
			return sandbox, nil
		}
		if daytonaStateFailed(state) {
			return nil, exit(5, "daytona sandbox %s entered terminal state=%s", id, state)
		}
		if time.Now().After(deadline) {
			return nil, exit(5, "timed out waiting for daytona sandbox %s (state=%s)", id, state)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func resolveDaytonaSandbox(ctx context.Context, client daytonaAPI, cfg Config, id string) (*daytona.Sandbox, string, error) {
	if id == "" {
		return nil, "", exit(2, "provider=daytona requires --id <sandbox-id-or-slug>")
	}
	sandboxes, err := client.ListCrabboxSandboxes(ctx)
	if err != nil {
		return nil, "", daytonaError("list sandboxes", err)
	}
	if isCanonicalLeaseID(id) {
		for i := range sandboxes {
			if sandboxes[i].Labels["lease"] == id {
				return &sandboxes[i], id, nil
			}
		}
	}
	slug := normalizeLeaseSlug(id)
	var matches []*daytona.Sandbox
	for i := range sandboxes {
		if slug != "" && normalizeLeaseSlug(sandboxes[i].Labels["slug"]) == slug {
			matches = append(matches, &sandboxes[i])
		}
	}
	if len(matches) > 1 {
		return nil, "", exit(4, "daytona slug %q matches multiple sandboxes", id)
	}
	if len(matches) == 1 {
		return matches[0], matches[0].Labels["lease"], nil
	}
	for i := range sandboxes {
		if sandboxes[i].GetId() == id || sandboxes[i].GetName() == id || sandboxes[i].Labels["lease_name"] == id {
			return &sandboxes[i], blank(sandboxes[i].Labels["lease"], id), nil
		}
	}
	if claim, ok, err := resolveLeaseClaim(id); err != nil {
		return nil, "", err
	} else if ok && claim.Provider == daytonaProvider {
		for i := range sandboxes {
			if sandboxes[i].Labels["lease"] == claim.LeaseID {
				return &sandboxes[i], claim.LeaseID, nil
			}
		}
	}
	sandbox, err := client.GetSandbox(ctx, id)
	if err == nil && sandbox != nil && sandbox.GetId() != "" {
		return sandbox, blank(sandbox.Labels["lease"], id), nil
	}
	_ = cfg
	return nil, "", exit(4, "daytona sandbox not found: %s", id)
}

func daytonaSSHTargetFor(ctx context.Context, client daytonaAPI, cfg Config, server Server) (SSHTarget, error) {
	access, err := client.CreateSSHAccess(ctx, server.CloudID, time.Duration(daytonaSSHAccessMinutes(cfg))*time.Minute)
	if err != nil {
		return SSHTarget{}, daytonaError("create ssh access", err)
	}
	return daytonaSSHTargetFromAccess(cfg, access)
}

func daytonaSSHTargetFromAccess(cfg Config, access daytonaSSHAccess) (SSHTarget, error) {
	user := strings.TrimSpace(access.Token)
	host := daytonaSSHGatewayHost(cfg)
	port := "22"
	if command := strings.TrimSpace(access.Command); command != "" {
		parsedUser, parsedHost, parsedPort, err := parseDaytonaSSHCommand(command)
		if err != nil {
			return SSHTarget{}, err
		}
		user = parsedUser
		host = parsedHost
		port = parsedPort
	}
	if user == "" {
		return SSHTarget{}, fmt.Errorf("daytona ssh access response missing token")
	}
	return SSHTarget{
		User:        user,
		Host:        host,
		Port:        port,
		Key:         "",
		TargetOS:    targetLinux,
		ReadyCheck:  "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null",
		AuthSecret:  true,
		NetworkKind: NetworkPublic,
	}, nil
}

func parseDaytonaSSHCommand(command string) (string, string, string, error) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "", "", "", fmt.Errorf("daytona ssh command is empty")
	}
	if fields[0] == "ssh" {
		fields = fields[1:]
	}
	port := "22"
	destination := ""
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		switch {
		case field == "-p":
			if i+1 >= len(fields) || strings.TrimSpace(fields[i+1]) == "" {
				return "", "", "", fmt.Errorf("daytona ssh command missing -p value: %q", command)
			}
			i++
			port = fields[i]
		case strings.HasPrefix(field, "-p") && len(field) > 2:
			port = strings.TrimPrefix(field, "-p")
		case strings.HasPrefix(field, "-"):
			return "", "", "", fmt.Errorf("daytona ssh command has unsupported option %q", field)
		default:
			destination = field
		}
	}
	user, host, ok := strings.Cut(destination, "@")
	if !ok || strings.TrimSpace(user) == "" || strings.TrimSpace(host) == "" {
		return "", "", "", fmt.Errorf("daytona ssh command missing user@host destination: %q", command)
	}
	return user, host, port, nil
}

func daytonaSandboxesToServers(sandboxes []daytona.Sandbox, cfg Config) []Server {
	servers := make([]Server, 0, len(sandboxes))
	for i := range sandboxes {
		servers = append(servers, daytonaSandboxToServer(&sandboxes[i], cfg))
	}
	return servers
}

func daytonaSandboxToServer(sandbox *daytona.Sandbox, cfg Config) Server {
	labels := map[string]string{}
	if sandbox != nil && sandbox.Labels != nil {
		for k, v := range sandbox.Labels {
			labels[k] = v
		}
	}
	server := Server{Provider: daytonaProvider, Labels: labels}
	if sandbox != nil {
		server.CloudID = sandbox.GetId()
		server.Name = sandbox.GetName()
		server.Status = daytonaSandboxState(sandbox)
	}
	if server.Name == "" {
		server.Name = blank(labels["lease_name"], server.CloudID)
	}
	server.ServerType.Name = blank(labels["server_type"], serverTypeForProviderClass(cfg.Provider, cfg.Class))
	return server
}

func daytonaSandboxState(sandbox *daytona.Sandbox) string {
	if sandbox == nil || sandbox.State == nil {
		return ""
	}
	return string(*sandbox.State)
}

func daytonaStateReady(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "started", "running", "ready", "active":
		return true
	default:
		return false
	}
}

func daytonaStateFailed(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "error", "errored", "failed", "build_failed", "destroyed", "deleted":
		return true
	default:
		return false
	}
}

func daytonaUser(cfg Config) string {
	return blank(strings.TrimSpace(cfg.Daytona.User), "daytona")
}

func daytonaWorkRoot(cfg Config) string {
	return blank(strings.TrimSpace(cfg.Daytona.WorkRoot), "/home/"+daytonaUser(cfg)+"/crabbox")
}

func daytonaSSHGatewayHost(cfg Config) string {
	return blank(strings.TrimSpace(cfg.Daytona.SSHGatewayHost), "ssh.app.daytona.io")
}

func daytonaSSHAccessMinutes(cfg Config) int {
	if cfg.Daytona.SSHAccessMinutes > 0 {
		return cfg.Daytona.SSHAccessMinutes
	}
	return 30
}

func redactedSSHUser(cfg Config, server Server, target SSHTarget) string {
	if target.AuthSecret {
		return daytonaTokenRedacted
	}
	if cfg.Provider == daytonaProvider || server.Provider == daytonaProvider {
		return daytonaTokenRedacted
	}
	return target.User
}

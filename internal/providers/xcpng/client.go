package xcpng

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type xapiClient struct {
	endpoint string
	session  string
	username string
	password string
	http     *http.Client
}

var (
	xcpNgShutdownPollInterval = 2 * time.Second
	xcpNgShutdownTimeout      = 5 * time.Minute
	xcpNgTaskPollInterval     = 1 * time.Second
	xcpNgTaskTimeout          = 1 * time.Minute
)

func newXAPIClient(ctx context.Context, cfg Config) (*xapiClient, error) {
	xcfg := xcpNgProviderConfig(cfg)
	if err := validateXCPNgConfig(xcfg); err != nil {
		return nil, err
	}
	endpoint, err := xapiEndpoint(xcfg.APIURL)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: xcfg.InsecureTLS}, //nolint:gosec // explicit private-lab opt-in.
	}
	httpClient := &http.Client{Timeout: 60 * time.Second, Transport: transport}
	c := &xapiClient{endpoint: endpoint, username: xcfg.Username, password: xcfg.Password, http: httpClient}
	if err := c.login(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *xapiClient) login(ctx context.Context) error {
	session, err := c.callRawString(ctx, "session.login_with_password", c.username, c.password, "1.0", "crabbox")
	if err != nil {
		if master := xapiMasterAddress(err); master != "" {
			redirected, redirectErr := xapiEndpointForMaster(c.endpoint, master)
			if redirectErr != nil {
				return fmt.Errorf("xcp-ng pool master redirect %q: %w", master, redirectErr)
			}
			c.endpoint = redirected
			session, err = c.callRawString(ctx, "session.login_with_password", c.username, c.password, "1.0", "crabbox")
		}
	}
	if err != nil {
		return err
	}
	c.session = session
	return nil
}

func (c *xapiClient) Close(ctx context.Context) error {
	if c.session == "" {
		return nil
	}
	_, err := c.call(ctx, "session.logout", c.session)
	if err != nil {
		return err
	}
	c.session = ""
	return nil
}

func (c *xapiClient) DoctorInventory(ctx context.Context, cfg xcpNgConfig) ([]Server, error) {
	_ = cfg
	return c.ListCrabboxServers(ctx)
}

func (c *xapiClient) ListCrabboxServers(ctx context.Context) ([]Server, error) {
	vms, err := c.vmRecords(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(vms))
	for _, vm := range vms {
		server := xcpNgVMToServer(vm, vm.Labels, "")
		if isCrabboxLease(server) {
			servers = append(servers, server)
		}
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	return servers, nil
}

func (c *xapiClient) ResolveTemplate(ctx context.Context, cfg xcpNgConfig) (xapiRef, error) {
	if cfg.TemplateUUID != "" {
		return c.getByUUID(ctx, "VM", cfg.TemplateUUID)
	}
	if cfg.Template != "" {
		return c.getUniqueByName(ctx, "VM", cfg.Template)
	}
	return "", exit(3, "xcp-ng template or template UUID is required")
}

func (c *xapiClient) ResolveSR(ctx context.Context, cfg xcpNgConfig) (xapiRef, error) {
	if cfg.SRUUID != "" {
		return c.getByUUID(ctx, "SR", cfg.SRUUID)
	}
	if cfg.SR != "" {
		return c.getUniqueByName(ctx, "SR", cfg.SR)
	}
	return "", exit(3, "xcp-ng sr or sr UUID is required")
}

func (c *xapiClient) ResolveNetwork(ctx context.Context, cfg xcpNgConfig) (xapiRef, error) {
	if cfg.NetworkUUID != "" {
		return c.getByUUID(ctx, "network", cfg.NetworkUUID)
	}
	if cfg.Network != "" {
		return c.getUniqueByName(ctx, "network", cfg.Network)
	}
	return "", nil
}

func (c *xapiClient) ResolveHost(ctx context.Context, cfg xcpNgConfig) (xapiRef, error) {
	if cfg.Host == "" {
		return "", nil
	}
	if looksLikeUUID(cfg.Host) {
		return c.getByUUID(ctx, "host", cfg.Host)
	}
	return c.getUniqueByName(ctx, "host", cfg.Host)
}

func (c *xapiClient) CloneVM(ctx context.Context, req xcpNgCloneRequest) (xapiVM, error) {
	name := leaseVMName(req.LeaseID, req.Slug)
	cloneMethod := "VM.clone"
	cloneArgs := []any{c.session, req.TemplateRef.value(), name}
	if req.SRRef != "" {
		cloneMethod = "VM.copy"
		cloneArgs = append(cloneArgs, req.SRRef.value())
	}
	ref, err := c.callString(ctx, cloneMethod, cloneArgs...)
	if err != nil {
		return xapiVM{}, err
	}
	if req.HostRef != "" {
		if _, err := c.call(ctx, "VM.set_affinity", c.session, ref, req.HostRef.value()); err != nil && req.HostRef != "" {
			_ = c.deleteServer(context.Background(), ref, true)
			return xapiVM{}, err
		}
	}
	if req.NetworkRef != "" {
		if err := c.setVMNetwork(ctx, ref, req.NetworkRef.value()); err != nil {
			_ = c.deleteServer(context.Background(), ref, true)
			return xapiVM{}, err
		}
	}
	if err := c.setVMLabels(ctx, ref, req.Labels); err != nil {
		_ = c.deleteServer(context.Background(), ref, true)
		return xapiVM{}, err
	}
	if _, err := c.call(ctx, "VM.provision", c.session, ref); err != nil {
		_ = c.deleteServer(context.Background(), ref, true)
		return xapiVM{}, err
	}
	if err := c.markAttachedUserDisks(ctx, ref, vmDiskLabels(req.Labels)); err != nil {
		_ = c.deleteServer(context.Background(), ref, true)
		return xapiVM{}, err
	}
	uuid, err := c.callString(ctx, "VM.get_uuid", c.session, ref)
	if err != nil {
		_ = c.deleteServer(context.Background(), ref, true)
		return xapiVM{}, err
	}
	return xapiVM{Ref: ref, UUID: uuid, Name: name, PowerState: "halted", Labels: req.Labels}, nil
}

func (c *xapiClient) AttachConfigDrive(ctx context.Context, req xcpNgConfigDriveRequest) (xcpNgConfigDrive, error) {
	if req.SRRef == "" {
		return xcpNgConfigDrive{}, exit(3, "xcp-ng sr or sr UUID is required for config-drive creation")
	}
	labels := configDriveLabels(req.Labels)
	image, err := buildConfigDriveImage(req.Payload)
	if err != nil {
		return xcpNgConfigDrive{}, err
	}
	name := configDriveName(req.LeaseID, req.Slug)
	vdiRef, err := c.callString(ctx, "VDI.create", c.session, map[string]any{
		"name_label":       name,
		"name_description": "Crabbox cloud-init config drive",
		"SR":               req.SRRef.value(),
		"virtual_size":     len(image),
		"type":             "user",
		"sharable":         false,
		"read_only":        false,
		"xenstore_data":    map[string]string{},
		"sm_config":        map[string]string{},
		"tags":             []string{},
		"other_config":     labels,
	})
	if err != nil {
		return xcpNgConfigDrive{}, err
	}
	drive := xcpNgConfigDrive{VDIRef: vdiRef, Name: name, Labels: labels}
	if err := c.importRawVDI(ctx, vdiRef, image); err != nil {
		_ = c.DeleteConfigDrive(context.Background(), drive)
		return xcpNgConfigDrive{}, err
	}
	vbdRef, err := c.callString(ctx, "VBD.create", c.session, map[string]any{
		"VM":                       req.VMRef.value(),
		"VDI":                      vdiRef,
		"userdevice":               "autodetect",
		"bootable":                 false,
		"mode":                     "RO",
		"type":                     "Disk",
		"empty":                    false,
		"unpluggable":              true,
		"qos_algorithm_type":       "",
		"qos_algorithm_params":     map[string]string{},
		"qos_supported_algorithms": []string{},
		"other_config":             labels,
	})
	if err != nil {
		_ = c.DeleteConfigDrive(context.Background(), drive)
		return xcpNgConfigDrive{}, err
	}
	drive.VBDRef = vbdRef
	return drive, nil
}

func (c *xapiClient) StartVM(ctx context.Context, ref xapiRef) error {
	_, err := c.call(ctx, "VM.start", c.session, ref.value(), false, false)
	return err
}

func (c *xapiClient) GuestIPv4(ctx context.Context, ref xapiRef) (string, error) {
	guestMetrics, err := c.callString(ctx, "VM.get_guest_metrics", c.session, ref.value())
	if err != nil {
		return "", err
	}
	value, err := c.call(ctx, "VM_guest_metrics.get_networks", c.session, guestMetrics)
	if err != nil {
		return "", err
	}
	networkMap := xmlValueToStringMap(value)
	keys := make([]string, 0, len(networkMap))
	for key := range networkMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if ip := usableIPv4(networkMap[key]); ip != "" {
			return ip, nil
		}
	}
	return "", errors.New("no guest ipv4 address reported by XCP-ng guest metrics")
}

func (c *xapiClient) GuestIPv4ForID(ctx context.Context, id string) (string, error) {
	ref, err := c.vmRefForID(ctx, id)
	if err != nil {
		return "", err
	}
	return c.GuestIPv4(ctx, xapiRef(ref))
}

func (c *xapiClient) GetServer(ctx context.Context, id string) (Server, error) {
	ref, err := c.vmRefForID(ctx, id)
	if err != nil {
		return Server{}, err
	}
	record, err := c.vmRecord(ctx, ref)
	if err != nil {
		return Server{}, err
	}
	ip, _ := c.GuestIPv4(ctx, xapiRef(record.Ref))
	return xcpNgVMToServer(record, record.Labels, ip), nil
}

func (c *xapiClient) SetLabels(ctx context.Context, id string, labels map[string]string) error {
	ref, err := c.vmRefForID(ctx, id)
	if err != nil {
		return err
	}
	return c.setVMLabels(ctx, ref, labels)
}

func (c *xapiClient) DeleteServer(ctx context.Context, id string) error {
	return c.deleteServer(ctx, id, false)
}

func (c *xapiClient) deleteServer(ctx context.Context, id string, forceDisks bool) error {
	ref, err := c.vmRefForID(ctx, id)
	if err != nil {
		return err
	}
	var drives []xcpNgConfigDrive
	var disks []xcpNgConfigDrive
	skipVDIs := map[string]struct{}{}
	server, getErr := c.GetServer(ctx, ref)
	if getErr != nil && !forceDisks {
		return getErr
	}
	if getErr == nil {
		if isCrabboxLease(server) {
			found, err := c.configDrivesForLease(ctx, server.Labels["lease"])
			if err != nil {
				return err
			}
			drives = found
			for _, drive := range drives {
				if drive.VDIRef != "" {
					skipVDIs[drive.VDIRef] = struct{}{}
				}
			}
			if !forceDisks {
				disks, err = c.attachedDestroyableDisks(ctx, ref, skipVDIs, server.Labels["lease"])
				if err != nil {
					return err
				}
			}
		} else if !forceDisks {
			return exit(4, "refusing to delete non-Crabbox xcp-ng VM: %s", id)
		}
	}
	if forceDisks {
		disks, err = c.attachedDestroyableDisks(ctx, ref, map[string]struct{}{}, "")
		if err != nil {
			return err
		}
	}
	if err := c.shutdownVM(ctx, ref); err != nil {
		return err
	}
	for _, drive := range drives {
		if err := c.DeleteConfigDrive(ctx, drive); err != nil {
			return err
		}
	}
	for _, disk := range disks {
		if err := c.DeleteConfigDrive(ctx, disk); err != nil {
			return err
		}
	}
	if _, err := c.call(ctx, "VM.destroy", c.session, ref); err != nil {
		return err
	}
	return nil
}

func (c *xapiClient) shutdownVM(ctx context.Context, ref string) error {
	state, err := c.callString(ctx, "VM.get_power_state", c.session, ref)
	if err != nil {
		return err
	}
	if state == "Halted" {
		return nil
	}
	cleanErr := c.callDiscard(ctx, "VM.clean_shutdown", c.session, ref)
	if cleanErr == nil {
		if err := c.waitForPowerState(ctx, ref, "Halted", xcpNgShutdownTimeout); err == nil {
			return nil
		} else {
			cleanErr = err
		}
	}
	if isXAPIFault(cleanErr, "VM_BAD_POWER_STATE") {
		state, err := c.callString(ctx, "VM.get_power_state", c.session, ref)
		if err != nil {
			return err
		}
		if state == "Halted" {
			return nil
		}
	}
	if err := c.callDiscard(ctx, "VM.hard_shutdown", c.session, ref); err != nil {
		return fmt.Errorf("xcp-ng clean shutdown failed: %v; hard shutdown failed: %w", cleanErr, err)
	}
	if err := c.waitForPowerState(ctx, ref, "Halted", xcpNgShutdownTimeout); err != nil {
		return fmt.Errorf("xcp-ng clean shutdown failed: %v; hard shutdown wait failed: %w", cleanErr, err)
	}
	return nil
}

func (c *xapiClient) waitForPowerState(ctx context.Context, ref, want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		state, err := c.callString(ctx, "VM.get_power_state", c.session, ref)
		if err != nil {
			return err
		}
		if state == want {
			return nil
		}
		if time.Now().After(deadline) {
			return exit(5, "timed out waiting for xcp-ng VM %s power_state=%s", ref, want)
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(xcpNgShutdownPollInterval):
		}
	}
}

func (c *xapiClient) DeleteConfigDrive(ctx context.Context, drive xcpNgConfigDrive) error {
	if drive.VBDRef != "" {
		if err := c.callDiscard(ctx, "VBD.unplug", c.session, drive.VBDRef); err != nil && !isNotFound(err) && !isAlreadyDetached(err) && !isNotUnpluggable(err) && !isXAPIHaltedPowerStateFault(err) {
			return err
		}
		if err := c.callDiscard(ctx, "VBD.destroy", c.session, drive.VBDRef); err != nil && !isNotFound(err) {
			return err
		}
	}
	if drive.VDIRef != "" {
		if err := c.callDiscard(ctx, "VDI.destroy", c.session, drive.VDIRef); err != nil && !isNotFound(err) {
			return err
		}
	}
	return nil
}

func (c *xapiClient) vmRefForID(ctx context.Context, id string) (string, error) {
	if looksLikeUUID(id) {
		resolved, err := c.getByUUID(ctx, "VM", id)
		if err != nil {
			return "", err
		}
		return resolved.value(), nil
	}
	return id, nil
}

func redactedURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	redacted := *u
	if redacted.User != nil {
		redacted.User = url.User("<redacted>")
	}
	q := redacted.Query()
	if q.Has("session_id") {
		q.Set("session_id", "<redacted>")
		redacted.RawQuery = q.Encode()
	}
	text := redacted.String()
	return strings.ReplaceAll(text, "%3Credacted%3E", "<redacted>")
}

var (
	sessionIDTextPattern = regexp.MustCompile(`(?i)(session_id=)[^&\s]+`)
	uuidTextPattern      = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

func urlUserinfoSecrets(u *url.URL) []string {
	if u == nil || u.User == nil {
		return nil
	}
	secrets := []string{u.User.String()}
	if password, ok := u.User.Password(); ok {
		secrets = append(secrets, u.User.Username()+":***", password)
	}
	secrets = append(secrets, u.User.Username())
	return secrets
}

func redactSessionIDText(text string) string {
	return sessionIDTextPattern.ReplaceAllString(text, `${1}<redacted>`)
}

func redactXAPISensitiveText(text string, secrets ...string) string {
	text = redactSessionIDText(text)
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, "<redacted>")
		if escaped := url.QueryEscape(secret); escaped != secret {
			text = strings.ReplaceAll(text, escaped, "<redacted>")
		}
		if escaped := url.PathEscape(secret); escaped != secret {
			text = strings.ReplaceAll(text, escaped, "<redacted>")
		}
		var xmlEscaped bytes.Buffer
		if err := xml.EscapeText(&xmlEscaped, []byte(secret)); err == nil {
			if escaped := xmlEscaped.String(); escaped != secret {
				text = strings.ReplaceAll(text, escaped, "<redacted>")
			}
		}
	}
	return text
}

func (c *xapiClient) configDrivesForLease(ctx context.Context, leaseID string) ([]xcpNgConfigDrive, error) {
	if strings.TrimSpace(leaseID) == "" {
		return nil, nil
	}
	value, err := c.call(ctx, "VDI.get_all_records", c.session)
	if err != nil {
		return nil, err
	}
	records := xmlValueToStructMap(value)
	drives := make([]xcpNgConfigDrive, 0)
	for ref, record := range records {
		labels := xmlStructStringMap(record, "other_config")
		if labels["crabbox"] != "true" || labels["created_by"] != "crabbox" || labels["provider"] != "xcp-ng" || labels["lease"] != leaseID || labels["resource"] != "config-drive" {
			continue
		}
		vbds := xmlStructStrings(record, "VBDs")
		drive := xcpNgConfigDrive{VDIRef: ref, Name: xmlStructString(record, "name_label"), Labels: labels}
		if len(vbds) > 0 {
			drive.VBDRef = vbds[0]
		}
		drives = append(drives, drive)
	}
	return drives, nil
}

func (c *xapiClient) attachedDestroyableDisks(ctx context.Context, vmRef string, skipVDIs map[string]struct{}, requiredLease string) ([]xcpNgConfigDrive, error) {
	value, err := c.call(ctx, "VM.get_VBDs", c.session, vmRef)
	if err != nil {
		return nil, err
	}
	disks := make([]xcpNgConfigDrive, 0)
	for _, vbdRef := range xmlValueToStrings(value) {
		recordValue, err := c.call(ctx, "VBD.get_record", c.session, vbdRef)
		if err != nil {
			return nil, err
		}
		record := xmlValueToStruct(recordValue)
		if xmlStructString(record, "empty") == "true" {
			continue
		}
		vdiRef := xmlStructString(record, "VDI")
		if vdiRef == "" || vdiRef == "OpaqueRef:NULL" {
			continue
		}
		if _, skip := skipVDIs[vdiRef]; skip {
			continue
		}
		vdiValue, err := c.call(ctx, "VDI.get_record", c.session, vdiRef)
		if err != nil {
			return nil, err
		}
		vdi := xmlValueToStruct(vdiValue)
		if xmlStructString(vdi, "read_only") == "true" || xmlStructString(vdi, "sharable") == "true" || xmlStructString(vdi, "type") != "user" {
			continue
		}
		labels := xmlStructStringMap(vdi, "other_config")
		if labels["resource"] == "config-drive" {
			continue
		}
		if requiredLease != "" && !isCrabboxVMDisk(labels, requiredLease) {
			continue
		}
		disks = append(disks, xcpNgConfigDrive{VDIRef: vdiRef, VBDRef: vbdRef, Name: xmlStructString(vdi, "name_label")})
	}
	return disks, nil
}

func (c *xapiClient) markAttachedUserDisks(ctx context.Context, vmRef string, labels map[string]string) error {
	disks, err := c.attachedDestroyableDisks(ctx, vmRef, map[string]struct{}{}, "")
	if err != nil {
		return err
	}
	for _, disk := range disks {
		if err := c.setVDIOtherConfig(ctx, disk.VDIRef, labels); err != nil {
			return err
		}
	}
	return nil
}

func (c *xapiClient) setVDIOtherConfig(ctx context.Context, ref string, values map[string]string) error {
	for key, value := range values {
		if err := c.callDiscard(ctx, "VDI.remove_from_other_config", c.session, ref, key); err != nil && !isMissingMapKey(err) {
			return err
		}
		if _, err := c.call(ctx, "VDI.add_to_other_config", c.session, ref, key, value); err != nil {
			return err
		}
	}
	return nil
}

func (c *xapiClient) importRawVDI(ctx context.Context, vdiRef string, image []byte) error {
	taskRef, err := c.callString(ctx, "task.create", c.session, "crabbox import config drive", "Import Crabbox cloud-init config drive")
	if err != nil {
		return err
	}
	defer func() { _ = c.callDiscard(context.Background(), "task.destroy", c.session, taskRef) }()

	u, err := url.Parse(c.endpoint)
	if err != nil {
		return err
	}
	u.Path = "/import_raw_vdi/"
	q := u.Query()
	q.Set("session_id", c.session)
	q.Set("task_id", taskRef)
	q.Set("vdi", vdiRef)
	q.Set("format", "raw")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(image))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := c.http.Do(req)
	if err != nil {
		secrets := append([]string{c.session}, urlUserinfoSecrets(u)...)
		return fmt.Errorf("upload xcp-ng config-drive %s to %s: %s", vdiRef, redactedURL(u), redactXAPISensitiveText(err.Error(), secrets...))
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		secrets := append([]string{c.session}, urlUserinfoSecrets(u)...)
		return xapiHTTPError{StatusCode: res.StatusCode, Body: redactXAPISensitiveText(strings.TrimSpace(string(data)), secrets...)}
	}
	return c.waitForTaskSuccess(ctx, taskRef)
}

func (c *xapiClient) waitForTaskSuccess(ctx context.Context, taskRef string) error {
	deadline := time.Now().Add(xcpNgTaskTimeout)
	for {
		status, err := c.callString(ctx, "task.get_status", c.session, taskRef)
		if err != nil {
			return err
		}
		switch status {
		case "success":
			return nil
		case "failure", "cancelled":
			value, err := c.call(ctx, "task.get_error_info", c.session, taskRef)
			if err != nil {
				return fmt.Errorf("xcp-ng upload task %s: %s", taskRef, status)
			}
			info := strings.Join(xmlValueToStrings(value), ": ")
			if info == "" {
				info = xmlValueToString(value)
			}
			info = redactXAPISensitiveText(info, c.xapiSecrets("task.get_error_info", c.session, taskRef)...)
			return fmt.Errorf("xcp-ng upload task %s: %s %s", taskRef, status, info)
		}
		if time.Now().After(deadline) {
			return exit(5, "timed out waiting for xcp-ng upload task %s", taskRef)
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(xcpNgTaskPollInterval):
		}
	}
}

func (c *xapiClient) getByUUID(ctx context.Context, class, uuid string) (xapiRef, error) {
	ref, err := c.callString(ctx, class+".get_by_uuid", c.session, uuid)
	if err != nil {
		return "", err
	}
	return xapiRef(ref), nil
}

func (c *xapiClient) getUniqueByName(ctx context.Context, class, name string) (xapiRef, error) {
	value, err := c.call(ctx, class+".get_by_name_label", c.session, name)
	if err != nil {
		return "", err
	}
	refs := xmlValueToStrings(value)
	if len(refs) == 0 {
		return "", exit(4, "xcp-ng %s not found by name: %s", class, name)
	}
	if len(refs) > 1 {
		return "", exit(4, "xcp-ng %s name is ambiguous: %s", class, name)
	}
	return xapiRef(refs[0]), nil
}

func (c *xapiClient) vmRecords(ctx context.Context) ([]xapiVM, error) {
	value, err := c.call(ctx, "VM.get_all_records", c.session)
	if err != nil {
		return nil, err
	}
	records := xmlValueToStructMap(value)
	vms := make([]xapiVM, 0, len(records))
	for ref, record := range records {
		if xmlStructString(record, "is_a_template") == "true" {
			continue
		}
		vms = append(vms, xapiVM{
			Ref:        ref,
			UUID:       xmlStructString(record, "uuid"),
			Name:       xapiName(xmlStructString(record, "name_label")),
			PowerState: xmlStructString(record, "power_state"),
			Labels:     parseRenderedLabels(xmlStructStringMap(record, "other_config")["crabbox:labels"]),
		})
	}
	return vms, nil
}

func (c *xapiClient) vmRecord(ctx context.Context, ref string) (xapiVM, error) {
	value, err := c.call(ctx, "VM.get_record", c.session, ref)
	if err != nil {
		return xapiVM{}, err
	}
	record := xmlValueToStruct(value)
	return xapiVM{
		Ref:        ref,
		UUID:       xmlStructString(record, "uuid"),
		Name:       xapiName(xmlStructString(record, "name_label")),
		PowerState: xmlStructString(record, "power_state"),
		Labels:     parseRenderedLabels(xmlStructStringMap(record, "other_config")["crabbox:labels"]),
	}, nil
}

func (c *xapiClient) setVMLabels(ctx context.Context, ref string, labels map[string]string) error {
	return c.setVMOtherConfig(ctx, ref, map[string]string{
		"crabbox:labels":     renderLabels(labels),
		"crabbox:managed":    "true",
		"crabbox:lease":      labels["lease"],
		"crabbox:created_by": "crabbox",
	})
}

func (c *xapiClient) setVMNetwork(ctx context.Context, vmRef, networkRef string) error {
	value, err := c.call(ctx, "VM.get_VIFs", c.session, vmRef)
	if err != nil {
		return err
	}
	for _, vifRef := range xmlValueToStrings(value) {
		if _, err := c.call(ctx, "VIF.move", c.session, vifRef, networkRef); err != nil {
			return err
		}
	}
	return nil
}

func (c *xapiClient) setVMOtherConfig(ctx context.Context, ref string, values map[string]string) error {
	for key, value := range values {
		if err := c.callDiscard(ctx, "VM.remove_from_other_config", c.session, ref, key); err != nil && !isMissingMapKey(err) {
			return err
		}
		if _, err := c.call(ctx, "VM.add_to_other_config", c.session, ref, key, value); err != nil {
			return err
		}
	}
	return nil
}

func (c *xapiClient) callString(ctx context.Context, method string, params ...any) (string, error) {
	value, err := c.call(ctx, method, params...)
	if err != nil {
		return "", err
	}
	return xmlValueToString(value), nil
}

func (c *xapiClient) callDiscard(ctx context.Context, method string, params ...any) error {
	_, err := c.call(ctx, method, params...)
	return err
}

func (c *xapiClient) call(ctx context.Context, method string, params ...any) (xmlRPCValue, error) {
	value, err := c.callRaw(ctx, method, params...)
	if err == nil || method == "session.login_with_password" || method == "session.logout" {
		return value, err
	}
	master := xapiMasterAddress(err)
	if master == "" {
		return value, err
	}
	redirected, redirectErr := xapiEndpointForMaster(c.endpoint, master)
	if redirectErr != nil {
		return xmlRPCValue{}, fmt.Errorf("xcp-ng pool master redirect %q: %w", master, redirectErr)
	}
	oldSession := c.session
	if oldSession != "" {
		logoutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, _ = c.callRaw(logoutCtx, "session.logout", oldSession)
		cancel()
	}
	c.endpoint = redirected
	c.session = ""
	if loginErr := c.login(ctx); loginErr != nil {
		return xmlRPCValue{}, loginErr
	}
	retryParams := append([]any(nil), params...)
	if len(retryParams) > 0 {
		if session, ok := retryParams[0].(string); ok && session == oldSession {
			retryParams[0] = c.session
		}
	}
	return c.callRaw(ctx, method, retryParams...)
}

func (c *xapiClient) callRawString(ctx context.Context, method string, params ...any) (string, error) {
	value, err := c.callRaw(ctx, method, params...)
	if err != nil {
		return "", err
	}
	return xmlValueToString(value), nil
}

func (c *xapiClient) callRaw(ctx context.Context, method string, params ...any) (xmlRPCValue, error) {
	body, err := encodeXMLRPCRequest(method, params...)
	if err != nil {
		return xmlRPCValue{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return xmlRPCValue{}, err
	}
	req.Header.Set("Content-Type", "text/xml")
	res, err := c.http.Do(req)
	if err != nil {
		secrets := urlUserinfoSecrets(req.URL)
		return xmlRPCValue{}, fmt.Errorf("xcp-ng XML-RPC %s to %s: %s", method, redactedURL(req.URL), redactXAPISensitiveText(err.Error(), secrets...))
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return xmlRPCValue{}, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return xmlRPCValue{}, xapiHTTPError{StatusCode: res.StatusCode, Body: redactXAPISensitiveText(strings.TrimSpace(string(data)), c.xapiSecrets(method, params...)...)}
	}
	var response xmlRPCResponse
	if err := xml.Unmarshal(data, &response); err != nil {
		return xmlRPCValue{}, err
	}
	secrets := c.xapiSecrets(method, params...)
	if response.Fault != nil {
		return xmlRPCValue{}, xapiFaultError{Value: response.Fault.Value, Secrets: secrets}
	}
	if len(response.Params) == 0 {
		return xmlRPCValue{}, nil
	}
	return unwrapXAPIResponse(response.Params[0].Value, secrets)
}

func (c *xapiClient) xapiSecrets(method string, params ...any) []string {
	secrets := []string{c.session}
	if method == "session.login_with_password" && len(params) > 1 {
		if password, ok := params[1].(string); ok {
			secrets = append(secrets, password)
		}
	}
	return secrets
}

type xapiHTTPError struct {
	StatusCode int
	Body       string
}

func (e xapiHTTPError) Error() string {
	return fmt.Sprintf("xcp-ng HTTP %d: %s", e.StatusCode, e.Body)
}

type xapiFaultError struct {
	Value   xmlRPCValue
	Secrets []string
}

func (e xapiFaultError) Error() string {
	fields := xmlValueToStringMap(e.Value)
	if text := fields["faultString"]; text != "" {
		return "xcp-ng fault: " + redactXAPISensitiveText(text, e.Secrets...)
	}
	return "xcp-ng fault"
}

type xapiStatusError struct {
	Fields  map[string]xmlRPCValue
	Secrets []string
}

func (e xapiStatusError) Error() string {
	var text string
	if values := xmlValueToStrings(e.Fields["ErrorDescription"]); len(values) > 0 {
		text = strings.Join(values, ": ")
	} else {
		text = xmlValueToString(e.Fields["ErrorDescription"])
	}
	if text == "" {
		return "xcp-ng failure"
	}
	return "xcp-ng failure: " + redactXAPISensitiveText(text, e.Secrets...)
}

func unwrapXAPIResponse(value xmlRPCValue, secrets []string) (xmlRPCValue, error) {
	fields := xmlValueToStruct(value)
	status := xmlValueToString(fields["Status"])
	if status == "" {
		return value, nil
	}
	if status != "Success" {
		return xmlRPCValue{}, xapiStatusError{Fields: fields, Secrets: secrets}
	}
	return fields["Value"], nil
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var httpErr xapiHTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
		return true
	}
	return strings.Contains(err.Error(), "HANDLE_INVALID") || strings.Contains(err.Error(), "not found")
}

func isMissingMapKey(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "MAP_KEY_NOT_FOUND") || strings.Contains(text, "key not found")
}

func isAlreadyDetached(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "DEVICE_ALREADY_DETACHED") || strings.Contains(text, "already detached")
}

func isNotUnpluggable(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "VBD_NOT_UNPLUGGABLE") || strings.Contains(text, "not unpluggable")
}

func isXAPIFault(err error, code string) bool {
	if err == nil || code == "" {
		return false
	}
	var statusErr xapiStatusError
	if errors.As(err, &statusErr) {
		for _, value := range xmlValueToStrings(statusErr.Fields["ErrorDescription"]) {
			if value == code {
				return true
			}
		}
	}
	var faultErr xapiFaultError
	if errors.As(err, &faultErr) {
		fields := xmlValueToStringMap(faultErr.Value)
		if strings.Contains(fields["faultString"], code) {
			return true
		}
	}
	return strings.Contains(err.Error(), code)
}

func isXAPIHaltedPowerStateFault(err error) bool {
	if !isXAPIFault(err, "VM_BAD_POWER_STATE") {
		return false
	}
	var statusErr xapiStatusError
	if errors.As(err, &statusErr) {
		values := xmlValueToStrings(statusErr.Fields["ErrorDescription"])
		if len(values) > 0 {
			return strings.EqualFold(values[len(values)-1], "halted")
		}
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "halted")
}

func xapiEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return "", exit(3, "xcp-ng api URL must use https; insecureTls only disables certificate verification")
	}
	if u.Host == "" {
		return "", exit(3, "xcp-ng api URL must include a host")
	}
	u.User = nil
	if u.Path == "" || u.Path == "/" {
		u.Path = "/"
	}
	return u.String(), nil
}

func xapiEndpointForMaster(current, master string) (string, error) {
	currentURL, err := url.Parse(current)
	if err != nil {
		return "", err
	}
	master = strings.Trim(strings.TrimSpace(master), " \t\r\n,;[]()")
	if master == "" {
		return "", exit(3, "xcp-ng pool master redirect did not include a host")
	}
	if strings.Contains(master, "://") {
		return xapiEndpoint(master)
	}
	if !hostPortHasPort(master) && currentURL.Port() != "" {
		master = net.JoinHostPort(master, currentURL.Port())
	}
	currentURL.Host = master
	if currentURL.Scheme == "http" && !isLoopbackHost(hostnameForEndpointHost(master)) {
		currentURL.Scheme = "https"
	}
	return xapiEndpoint(currentURL.String())
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func hostnameForEndpointHost(host string) string {
	u, err := url.Parse("//" + host)
	if err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func hostPortHasPort(host string) bool {
	u, err := url.Parse("//" + host)
	return err == nil && u.Port() != ""
}

func xapiMasterAddress(err error) string {
	var statusErr xapiStatusError
	if errors.As(err, &statusErr) {
		values := xmlValueToStrings(statusErr.Fields["ErrorDescription"])
		if len(values) >= 2 && values[0] == "HOST_IS_SLAVE" {
			return strings.TrimSpace(values[1])
		}
		if value := parseHostIsSlaveMaster(strings.Join(values, " ")); value != "" {
			return value
		}
	}
	var faultErr xapiFaultError
	if errors.As(err, &faultErr) {
		fields := xmlValueToStringMap(faultErr.Value)
		if value := parseHostIsSlaveMaster(fields["faultString"]); value != "" {
			return value
		}
	}
	return ""
}

func parseHostIsSlaveMaster(text string) string {
	fields := strings.Fields(text)
	for i, field := range fields {
		field = strings.Trim(field, " \t\r\n:,;[]()")
		if field == "HOST_IS_SLAVE" && i+1 < len(fields) {
			return strings.Trim(fields[i+1], " \t\r\n,;[]()")
		}
	}
	return ""
}

func leaseVMName(leaseID, slug string) string {
	return "crabbox-" + strings.TrimPrefix(core.NormalizeLeaseSlug(slug), "crabbox-") + "-" + strings.TrimPrefix(leaseID, "cbx_")
}

func configDriveName(leaseID, slug string) string {
	return leaseVMName(leaseID, slug) + "-config"
}

func looksLikeUUID(value string) bool {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "OpaqueRef:") {
		return false
	}
	return uuidTextPattern.MatchString(value)
}

func usableIPv4(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "127.") {
		return ""
	}
	ip := net.ParseIP(value)
	if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return ""
	}
	return value
}

func renderLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&b, "%s=%s\n", key, labels[key])
	}
	return b.String()
}

func parseRenderedLabels(value string) map[string]string {
	labels := map[string]string{}
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		labels[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	return labels
}

func encodeXMLRPCRequest(method string, params ...any) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0"?>`)
	b.WriteString("<methodCall><methodName>")
	xml.EscapeText(&b, []byte(method))
	b.WriteString("</methodName><params>")
	for _, param := range params {
		b.WriteString("<param><value>")
		encodeXMLRPCValue(&b, param)
		b.WriteString("</value></param>")
	}
	b.WriteString("</params></methodCall>")
	return b.Bytes(), nil
}

func encodeXMLRPCValue(b *bytes.Buffer, value any) {
	switch v := value.(type) {
	case string:
		b.WriteString("<string>")
		xml.EscapeText(b, []byte(v))
		b.WriteString("</string>")
	case int:
		fmt.Fprintf(b, "<int>%d</int>", v)
	case int64:
		fmt.Fprintf(b, "<int>%d</int>", v)
	case bool:
		if v {
			b.WriteString("<boolean>1</boolean>")
		} else {
			b.WriteString("<boolean>0</boolean>")
		}
	case []string:
		b.WriteString("<array><data>")
		for _, item := range v {
			b.WriteString("<value>")
			encodeXMLRPCValue(b, item)
			b.WriteString("</value>")
		}
		b.WriteString("</data></array>")
	case map[string]string:
		b.WriteString("<struct>")
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			b.WriteString("<member><name>")
			xml.EscapeText(b, []byte(key))
			b.WriteString("</name><value>")
			encodeXMLRPCValue(b, v[key])
			b.WriteString("</value></member>")
		}
		b.WriteString("</struct>")
	case map[string]any:
		b.WriteString("<struct>")
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			b.WriteString("<member><name>")
			xml.EscapeText(b, []byte(key))
			b.WriteString("</name><value>")
			encodeXMLRPCValue(b, v[key])
			b.WriteString("</value></member>")
		}
		b.WriteString("</struct>")
	default:
		b.WriteString("<string>")
		xml.EscapeText(b, []byte(fmt.Sprint(v)))
		b.WriteString("</string>")
	}
}

type xmlRPCResponse struct {
	Params []struct {
		Value xmlRPCValue `xml:"value"`
	} `xml:"params>param"`
	Fault *struct {
		Value xmlRPCValue `xml:"value"`
	} `xml:"fault"`
}

type xmlRPCValue struct {
	CharData string         `xml:",chardata"`
	Kind     string         `xml:",any"`
	String   string         `xml:"string"`
	Int      string         `xml:"int"`
	I4       string         `xml:"i4"`
	Boolean  string         `xml:"boolean"`
	Array    []xmlRPCValue  `xml:"array>data>value"`
	Struct   []xmlRPCMember `xml:"struct>member"`
}

type xmlRPCMember struct {
	Name  string      `xml:"name"`
	Value xmlRPCValue `xml:"value"`
}

func xmlValueToString(value xmlRPCValue) string {
	switch {
	case value.String != "":
		return value.String
	case value.Int != "":
		return value.Int
	case value.I4 != "":
		return value.I4
	case value.Boolean != "":
		if value.Boolean == "1" {
			return "true"
		}
		return "false"
	}
	return strings.TrimSpace(value.CharData)
}

func xmlValueToStrings(value xmlRPCValue) []string {
	out := make([]string, 0, len(value.Array))
	for _, item := range value.Array {
		if text := xmlValueToString(item); text != "" {
			out = append(out, text)
		}
	}
	return out
}

func xmlValueToStruct(value xmlRPCValue) map[string]xmlRPCValue {
	out := map[string]xmlRPCValue{}
	for _, member := range value.Struct {
		out[member.Name] = member.Value
	}
	return out
}

func xmlValueToStructMap(value xmlRPCValue) map[string]map[string]xmlRPCValue {
	out := map[string]map[string]xmlRPCValue{}
	for _, member := range value.Struct {
		out[member.Name] = xmlValueToStruct(member.Value)
	}
	return out
}

func xmlValueToStringMap(value xmlRPCValue) map[string]string {
	out := map[string]string{}
	for _, member := range value.Struct {
		out[member.Name] = xmlValueToString(member.Value)
	}
	return out
}

func xmlStructString(record map[string]xmlRPCValue, key string) string {
	return xmlValueToString(record[key])
}

func xmlStructStrings(record map[string]xmlRPCValue, key string) []string {
	return xmlValueToStrings(record[key])
}

func xmlStructStringMap(record map[string]xmlRPCValue, key string) map[string]string {
	return xmlValueToStringMap(record[key])
}

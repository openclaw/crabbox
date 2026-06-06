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
	http     *http.Client
}

var (
	xcpNgShutdownPollInterval = 2 * time.Second
	xcpNgShutdownTimeout      = 5 * time.Minute
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
	c := &xapiClient{endpoint: endpoint, http: httpClient}
	session, err := c.callString(ctx, "session.login_with_password", xcfg.Username, xcfg.Password, "1.0", "crabbox")
	if err != nil {
		return nil, err
	}
	c.session = session
	return c, nil
}

func (c *xapiClient) Close(ctx context.Context) error {
	if c.session == "" {
		return nil
	}
	_, err := c.call(ctx, "session.logout", c.session)
	c.session = ""
	return err
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
		"VM":           req.VMRef.value(),
		"VDI":          vdiRef,
		"userdevice":   "autodetect",
		"bootable":     false,
		"mode":         "RO",
		"type":         "Disk",
		"empty":        false,
		"unpluggable":  true,
		"other_config": labels,
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
	managed := false
	if server, err := c.GetServer(ctx, ref); err == nil && isCrabboxLease(server) {
		managed = true
		found, err := c.configDrivesForLease(ctx, server.Labels["lease"])
		if err != nil {
			return err
		}
		drives = found
		skipVDIs := map[string]struct{}{}
		for _, drive := range drives {
			if drive.VDIRef != "" {
				skipVDIs[drive.VDIRef] = struct{}{}
			}
		}
		disks, err = c.attachedDestroyableDisks(ctx, ref, skipVDIs)
		if err != nil {
			return err
		}
	}
	if forceDisks && !managed {
		disks, err = c.attachedDestroyableDisks(ctx, ref, map[string]struct{}{})
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
		if err := c.callDiscard(ctx, "VBD.unplug", c.session, drive.VBDRef); err != nil && !isNotFound(err) && !isAlreadyDetached(err) {
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
	q := redacted.Query()
	if q.Has("session_id") {
		q.Set("session_id", "<redacted>")
		redacted.RawQuery = q.Encode()
	}
	return redacted.String()
}

var sessionIDTextPattern = regexp.MustCompile(`(?i)(session_id=)[^&\s]+`)

func redactSessionIDText(text string) string {
	return sessionIDTextPattern.ReplaceAllString(text, `${1}<redacted>`)
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

func (c *xapiClient) attachedDestroyableDisks(ctx context.Context, vmRef string, skipVDIs map[string]struct{}) ([]xcpNgConfigDrive, error) {
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
		if xmlStructStringMap(vdi, "other_config")["resource"] == "config-drive" {
			continue
		}
		disks = append(disks, xcpNgConfigDrive{VDIRef: vdiRef, VBDRef: vbdRef, Name: xmlStructString(vdi, "name_label")})
	}
	return disks, nil
}

func (c *xapiClient) importRawVDI(ctx context.Context, vdiRef string, image []byte) error {
	u, err := url.Parse(c.endpoint)
	if err != nil {
		return err
	}
	u.Path = "/import_raw_vdi/"
	q := u.Query()
	q.Set("session_id", c.session)
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
		return fmt.Errorf("upload xcp-ng config-drive %s to %s: %s", vdiRef, redactedURL(u), redactSessionIDText(err.Error()))
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return xapiHTTPError{StatusCode: res.StatusCode, Body: redactSessionIDText(strings.TrimSpace(string(data)))}
	}
	return nil
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
		if _, err := c.call(ctx, "VIF.set_network", c.session, vifRef, networkRef); err != nil {
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
		return xmlRPCValue{}, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return xmlRPCValue{}, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return xmlRPCValue{}, xapiHTTPError{StatusCode: res.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	var response xmlRPCResponse
	if err := xml.Unmarshal(data, &response); err != nil {
		return xmlRPCValue{}, err
	}
	if response.Fault != nil {
		return xmlRPCValue{}, xapiFaultError{Value: response.Fault.Value}
	}
	if len(response.Params) == 0 {
		return xmlRPCValue{}, nil
	}
	return unwrapXAPIResponse(response.Params[0].Value)
}

type xapiHTTPError struct {
	StatusCode int
	Body       string
}

func (e xapiHTTPError) Error() string {
	return fmt.Sprintf("xcp-ng HTTP %d: %s", e.StatusCode, e.Body)
}

type xapiFaultError struct {
	Value xmlRPCValue
}

func (e xapiFaultError) Error() string {
	fields := xmlValueToStringMap(e.Value)
	if text := fields["faultString"]; text != "" {
		return "xcp-ng fault: " + text
	}
	return "xcp-ng fault"
}

type xapiStatusError struct {
	Fields map[string]xmlRPCValue
}

func (e xapiStatusError) Error() string {
	if values := xmlValueToStrings(e.Fields["ErrorDescription"]); len(values) > 0 {
		return "xcp-ng failure: " + strings.Join(values, ": ")
	}
	if text := xmlValueToString(e.Fields["ErrorDescription"]); text != "" {
		return "xcp-ng failure: " + text
	}
	return "xcp-ng failure"
}

func unwrapXAPIResponse(value xmlRPCValue) (xmlRPCValue, error) {
	fields := xmlValueToStruct(value)
	status := xmlValueToString(fields["Status"])
	if status == "" {
		return value, nil
	}
	if status != "Success" {
		return xmlRPCValue{}, xapiStatusError{Fields: fields}
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
	if u.Path == "" || u.Path == "/" {
		u.Path = "/"
	}
	return u.String(), nil
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func leaseVMName(leaseID, slug string) string {
	return "crabbox-" + strings.TrimPrefix(core.NormalizeLeaseSlug(slug), "crabbox-") + "-" + strings.TrimPrefix(leaseID, "cbx_")
}

func configDriveName(leaseID, slug string) string {
	return leaseVMName(leaseID, slug) + "-config"
}

func looksLikeUUID(value string) bool {
	value = strings.TrimSpace(value)
	return len(value) >= 32 && strings.Count(value, "-") >= 4
}

func usableIPv4(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "127.") {
		return ""
	}
	ip := net.ParseIP(value)
	if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() {
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

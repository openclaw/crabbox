package xcpng

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type xapiClient struct {
	endpoint  string
	session   string
	username  string
	password  string
	guestCIDR string
	http      *http.Client
}

type guestProbeConfigError struct{ message string }

func (e guestProbeConfigError) Error() string { return e.message }

var (
	xcpNgShutdownPollInterval = 2 * time.Second
	xcpNgShutdownTimeout      = 5 * time.Minute
	xcpNgStartPollInterval    = 500 * time.Millisecond
	xcpNgStartTimeout         = 1 * time.Minute
	xcpNgTaskPollInterval     = 1 * time.Second
	xcpNgTaskTimeout          = 1 * time.Minute
	xcpNgRequestTimeout       = 5 * time.Minute
	xcpNgLongRequestTimeout   = 90 * time.Minute
	xcpNgProbeTCPAddress      = func(ctx context.Context, address string, timeout time.Duration) error {
		conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", address)
		if err == nil {
			_ = conn.Close()
		}
		return err
	}
	xcpNgLocalIPv4Networks  = localIPv4Networks
	xcpNgReadARPTable       = readARPTable
	xcpNgRunNeighborCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
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
	httpClient := newXAPIHTTPClient(xcfg.InsecureTLS)
	c := &xapiClient{
		endpoint:  endpoint,
		username:  xcfg.Username,
		password:  xcfg.Password,
		guestCIDR: strings.TrimSpace(os.Getenv("CRABBOX_XCP_NG_GUEST_CIDR")),
		http:      httpClient,
	}
	if err := c.login(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func newXAPIHTTPClient(insecureTLS bool) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 30 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: insecureTLS}, //nolint:gosec // explicit private-lab opt-in.
	}
	return &http.Client{Transport: transport}
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
	var (
		ref xapiRef
		err error
	)
	if cfg.TemplateUUID != "" {
		ref, err = c.getByUUID(ctx, "VM", cfg.TemplateUUID)
	} else if cfg.Template != "" {
		ref, err = c.getUniqueByName(ctx, "VM", cfg.Template)
	} else {
		return "", exit(3, "xcp-ng template or template UUID is required")
	}
	if err != nil {
		return "", err
	}
	isTemplate, err := c.callString(ctx, "VM.get_is_a_template", c.session, ref.value())
	if err != nil {
		return "", fmt.Errorf("validate xcp-ng template: %w", err)
	}
	if isTemplate != "true" {
		return "", exit(4, "xcp-ng VM is not a template: %s", firstNonBlank(cfg.TemplateUUID, cfg.Template))
	}
	return ref, nil
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

func (c *xapiClient) ResolveISOMedia(ctx context.Context, cfg xcpNgConfig, value string) (xcpNgISOMediaRef, error) {
	_ = cfg
	value = strings.TrimSpace(value)
	if value == "" {
		return xcpNgISOMediaRef{}, exit(3, "xcp-ng installer ISO is required")
	}
	if strings.HasPrefix(value, "OpaqueRef:") {
		return c.isoByRef(ctx, value, value)
	}
	if looksLikeUUID(value) {
		ref, err := c.getByUUID(ctx, "VDI", value)
		if err != nil {
			return xcpNgISOMediaRef{}, err
		}
		return c.isoByRef(ctx, ref.value(), value)
	}
	if fileExists(value) {
		return xcpNgISOMediaRef{Source: "local-file", NameLabel: value}, nil
	}
	ref, err := c.getUniqueByName(ctx, "VDI", value)
	if err != nil {
		return xcpNgISOMediaRef{}, err
	}
	return c.isoByRef(ctx, ref.value(), value)
}

func (c *xapiClient) isoByRef(ctx context.Context, ref, requested string) (xcpNgISOMediaRef, error) {
	value, err := c.call(ctx, "VDI.get_record", c.session, ref)
	if err != nil {
		return xcpNgISOMediaRef{}, err
	}
	record := xmlValueToStruct(value)
	iso := xcpNgISOMediaRef{
		VDIRef:    ref,
		UUID:      xmlStructString(record, "uuid"),
		NameLabel: xmlStructString(record, "name_label"),
		Source:    "sr-vdi",
	}
	if iso.NameLabel == "" {
		iso.NameLabel = requested
	}
	if xmlStructString(record, "is_tools_iso") == "true" {
		return xcpNgISOMediaRef{}, exit(4, "xcp-ng installer ISO must not be the Xen tools ISO: %s", requested)
	}
	if xmlStructString(record, "type") == "system" {
		return xcpNgISOMediaRef{}, exit(4, "xcp-ng installer ISO VDI is not user media: %s", requested)
	}
	return iso, nil
}

func (c *xapiClient) findBuiltinTemplate(ctx context.Context, nameLabel string) (string, error) {
	refs, err := c.call(ctx, "VM.get_by_name_label", c.session, nameLabel)
	if err != nil {
		return "", fmt.Errorf("VM.get_by_name_label %q: %w", nameLabel, err)
	}
	strings_ := xmlValueToStrings(refs)
	for _, ref := range strings_ {
		isTemplate, err := c.callString(ctx, "VM.get_is_a_template", c.session, ref)
		if err != nil {
			continue
		}
		if isTemplate == "true" {
			return ref, nil
		}
	}
	return "", fmt.Errorf("no builtin template named %q found", nameLabel)
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
	vm := xapiVM{Ref: ref, Name: name, PowerState: "halted", Labels: req.Labels}
	if err := c.setVMLabels(ctx, ref, req.Labels); err != nil {
		return c.rollbackClonedVM(ctx, ref, vm, req.Labels, false, err)
	}
	if req.HostRef != "" {
		if _, err := c.call(ctx, "VM.set_affinity", c.session, ref, req.HostRef.value()); err != nil && req.HostRef != "" {
			return c.rollbackClonedVM(ctx, ref, vm, req.Labels, true, err)
		}
	}
	if req.NetworkRef != "" {
		if err := c.setVMNetwork(ctx, ref, req.NetworkRef.value()); err != nil {
			return c.rollbackClonedVM(ctx, ref, vm, req.Labels, true, err)
		}
	}
	if _, err := c.call(ctx, "VM.provision", c.session, ref); err != nil {
		return c.rollbackClonedVM(ctx, ref, vm, req.Labels, true, err)
	}
	if err := c.markAttachedUserDisks(ctx, ref, vmDiskLabels(req.Labels)); err != nil {
		return c.rollbackClonedVM(ctx, ref, vm, req.Labels, true, err)
	}
	uuid, err := c.callString(ctx, "VM.get_uuid", c.session, ref)
	if err != nil {
		return c.rollbackClonedVM(ctx, ref, vm, req.Labels, true, err)
	}
	vm.UUID = uuid
	return vm, nil
}

func (c *xapiClient) rollbackClonedVM(ctx context.Context, ref string, vm xapiVM, labels map[string]string, labeled bool, cause error) (xapiVM, error) {
	cleanupCtx, cancel := xcpNgRollbackContext(ctx)
	defer cancel()
	cleanupErr := c.deleteServer(cleanupCtx, ref, true)
	if cleanupErr == nil {
		return xapiVM{}, cause
	}
	if !labeled {
		recoveryCtx, recoveryCancel := xcpNgRollbackContext(ctx)
		defer recoveryCancel()
		if recoveryErr := c.setVMLabels(recoveryCtx, ref, labels); recoveryErr != nil {
			return vm, errors.Join(cause, fmt.Errorf("rollback copied xcp-ng VM: %w", cleanupErr), fmt.Errorf("mark copied xcp-ng VM for recovery: %w", recoveryErr))
		}
	}
	return vm, errors.Join(cause, fmt.Errorf("rollback copied xcp-ng VM: %w", cleanupErr))
}

func (c *xapiClient) CreateFreshVM(ctx context.Context, req xcpNgFreshVMRequest) (xcpNgFreshVMResult, error) {
	memoryBytes := req.MemoryBytes
	if memoryBytes <= 0 {
		memoryBytes = 4 * 1024 * 1024 * 1024
	}
	vcpusMax := req.VCPUsMax
	if vcpusMax <= 0 {
		vcpusMax = 2
	}
	vcpusStartup := req.VCPUsStart
	if vcpusStartup <= 0 {
		vcpusStartup = vcpusMax
	}
	hvmBootParams := map[string]string{"order": "dc"}
	for key, value := range req.HVMBoot {
		hvmBootParams[key] = value
	}
	if req.SecureBoot && hvmBootParams["firmware"] == "" {
		hvmBootParams["firmware"] = "uefi"
	}
	platformKeys := map[string]string{}
	for key, value := range req.Platform {
		platformKeys[key] = value
	}
	if req.SecureBoot {
		platformKeys["secureboot"] = "true"
		platformKeys["device-model"] = "qemu-upstream-uefi"
	}
	// Clone from the "Other install media" template instead of using VM.create,
	// which requires Map(String,String) fields that trigger the XAPI XML-RPC
	// hashtbl_xml unmarshaller. Setting individual keys via VM.add_to_* avoids
	// the encoding mismatch entirely.
	templateRef, err := c.findBuiltinTemplate(ctx, "Other install media")
	if err != nil {
		return xcpNgFreshVMResult{}, fmt.Errorf("find HVM template for fresh VM: %w", err)
	}
	ref, err := c.callString(ctx, "VM.clone", c.session, templateRef, req.Name)
	if err != nil {
		return xcpNgFreshVMResult{}, err
	}
	result := xcpNgFreshVMResult{VM: xapiVM{Ref: ref, Name: req.Name, PowerState: "halted", Labels: req.Labels}}
	labeled := false
	rollback := func(cause error) (xcpNgFreshVMResult, error) {
		return c.rollbackFreshVM(ctx, result, req.Labels, labeled, cause)
	}
	if _, err := c.call(ctx, "VM.set_is_a_template", c.session, ref, false); err != nil {
		return rollback(err)
	}
	// If the template has no disks and we need an SR for provisioning, use VM.copy
	// For now, VM.clone is sufficient since we attach disks separately.
	// Configure the cloned VM: set memory, VCPUs, boot params, and platform keys
	if _, err := c.call(ctx, "VM.set_memory_static_max", c.session, ref, memoryBytes); err != nil {
		return rollback(err)
	}
	if _, err := c.call(ctx, "VM.set_memory_dynamic_max", c.session, ref, memoryBytes); err != nil {
		return rollback(err)
	}
	if _, err := c.call(ctx, "VM.set_memory_dynamic_min", c.session, ref, memoryBytes); err != nil {
		return rollback(err)
	}
	if _, err := c.call(ctx, "VM.set_memory_static_min", c.session, ref, memoryBytes); err != nil {
		return rollback(err)
	}
	if _, err := c.call(ctx, "VM.set_VCPUs_max", c.session, ref, vcpusMax); err != nil {
		return rollback(err)
	}
	if _, err := c.call(ctx, "VM.set_VCPUs_at_startup", c.session, ref, vcpusStartup); err != nil {
		return rollback(err)
	}
	// Remove default HVM boot params inherited from the template, then set our own
	existingBootParams, err := c.callStringMap(ctx, "VM.get_HVM_boot_params", c.session, ref)
	if err != nil {
		return rollback(err)
	}
	mergedBootParams := map[string]string{}
	for key, value := range existingBootParams {
		mergedBootParams[key] = value
	}
	for key, value := range hvmBootParams {
		mergedBootParams[key] = value
	}
	for key, existingValue := range existingBootParams {
		if value, ok := mergedBootParams[key]; !ok || value != existingValue {
			c.callDiscard(ctx, "VM.remove_from_HVM_boot_params", c.session, ref, key)
		}
	}
	for key, value := range mergedBootParams {
		if existingValue, ok := existingBootParams[key]; ok && existingValue == value {
			continue
		}
		if _, err := c.call(ctx, "VM.add_to_HVM_boot_params", c.session, ref, key, value); err != nil {
			return rollback(err)
		}
	}
	existingPlatform, err := c.callStringMap(ctx, "VM.get_platform", c.session, ref)
	if err != nil {
		return rollback(err)
	}
	mergedPlatform := map[string]string{}
	for key, value := range existingPlatform {
		mergedPlatform[key] = value
	}
	for key, value := range platformKeys {
		mergedPlatform[key] = value
	}
	for key, existingValue := range existingPlatform {
		if value, ok := mergedPlatform[key]; !ok || value != existingValue {
			c.callDiscard(ctx, "VM.remove_from_platform", c.session, ref, key)
		}
	}
	for key, value := range mergedPlatform {
		if existingValue, ok := existingPlatform[key]; ok && existingValue == value {
			continue
		}
		if _, err := c.call(ctx, "VM.add_to_platform", c.session, ref, key, value); err != nil {
			return rollback(err)
		}
	}
	if req.SecureBoot {
		if _, err := c.call(ctx, "VM.set_HVM_boot_policy", c.session, ref, "BIOS order"); err != nil {
			return rollback(err)
		}
	}
	if req.VTPM {
		vtpmRef, err := c.callString(ctx, "VTPM.create", c.session, ref, true)
		if err != nil {
			return rollback(err)
		}
		result.VTPMRef = vtpmRef
	}
	if req.HostRef != "" {
		if _, err := c.call(ctx, "VM.set_affinity", c.session, ref, req.HostRef.value()); err != nil {
			return rollback(err)
		}
	}
	if err := c.setVMLabels(ctx, ref, req.Labels); err != nil {
		return rollback(err)
	}
	labeled = true
	if req.Network != nil && req.Network.NetworkRef != "" {
		vifRef, err := c.callString(ctx, "VIF.create", c.session, map[string]any{
			"device":               firstNonBlank(req.Network.Device, "0"),
			"network":              req.Network.NetworkRef.value(),
			"VM":                   ref,
			"MAC":                  firstNonBlank(req.Network.MAC, ""),
			"MTU":                  req.Network.MTU,
			"other_config":         req.Network.Labels,
			"currently_attached":   false,
			"qos_algorithm_type":   "",
			"qos_algorithm_params": map[string]string{},
		})
		if err != nil {
			return rollback(err)
		}
		result.VIFRef = vifRef
	}
	uuid, err := c.callString(ctx, "VM.get_uuid", c.session, ref)
	if err != nil {
		return rollback(err)
	}
	result.VM.UUID = uuid
	return result, nil
}

func (c *xapiClient) rollbackFreshVM(ctx context.Context, result xcpNgFreshVMResult, labels map[string]string, labeled bool, cause error) (xcpNgFreshVMResult, error) {
	cleanupCtx, cancel := xcpNgRollbackContext(ctx)
	defer cancel()
	cleanupErr := c.deleteServer(cleanupCtx, result.VM.Ref, true, result.VTPMRef)
	if cleanupErr == nil {
		return xcpNgFreshVMResult{}, cause
	}
	if !labeled {
		recoveryCtx, recoveryCancel := xcpNgRollbackContext(ctx)
		defer recoveryCancel()
		if recoveryErr := c.setVMLabels(recoveryCtx, result.VM.Ref, labels); recoveryErr != nil {
			return result, errors.Join(cause, fmt.Errorf("rollback fresh xcp-ng VM: %w", cleanupErr), fmt.Errorf("mark fresh xcp-ng VM for recovery: %w", recoveryErr))
		}
	}
	return result, errors.Join(cause, fmt.Errorf("rollback fresh xcp-ng VM: %w", cleanupErr))
}

func (c *xapiClient) rollbackConfigDrive(ctx context.Context, drive xcpNgConfigDrive, cause error) (xcpNgConfigDrive, error) {
	cleanupCtx, cancel := xcpNgRollbackContext(ctx)
	defer cancel()
	cleanupErr := c.DeleteConfigDrive(cleanupCtx, drive)
	if cleanupErr == nil {
		return xcpNgConfigDrive{}, cause
	}
	return drive, errors.Join(cause, fmt.Errorf("rollback xcp-ng drive %s: %w", drive.VDIRef, cleanupErr))
}

func xcpNgRollbackContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), xcpNgPartialRollbackTimeout)
}

func (c *xapiClient) ImportISO(ctx context.Context, req xcpNgImportISORequest) (xcpNgConfigDrive, error) {
	if req.SRRef == "" {
		return xcpNgConfigDrive{}, exit(3, "xcp-ng sr or sr UUID is required for ISO import")
	}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		return xcpNgConfigDrive{}, exit(3, "xcp-ng ISO import path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return xcpNgConfigDrive{}, exit(4, "xcp-ng ISO file not found: %s", path)
		}
		return xcpNgConfigDrive{}, exit(3, "stat xcp-ng ISO file %s: %v", path, err)
	}
	if info.IsDir() {
		return xcpNgConfigDrive{}, exit(4, "xcp-ng ISO path must be a file: %s", path)
	}
	labels := isoMediaLabels(req.Labels)
	name := firstNonBlank(strings.TrimSpace(req.Name), filepath.Base(path))
	description := firstNonBlank(strings.TrimSpace(req.Description), "Crabbox imported installer media")
	vdiRef, err := c.callString(ctx, "VDI.create", c.session, map[string]any{
		"name_label":       name,
		"name_description": description,
		"SR":               req.SRRef.value(),
		"virtual_size":     info.Size(),
		"type":             "user",
		"sharable":         false,
		"read_only":        false,
		"xenstore_data":    map[string]string{},
		"sm_config":        map[string]string{"type": "raw"},
		"tags":             []string{},
		"other_config":     labels,
	})
	if err != nil {
		return xcpNgConfigDrive{}, err
	}
	drive := xcpNgConfigDrive{VDIRef: vdiRef, Name: name, Labels: labels, DestroyVDI: req.DestroyVDI}
	if err := c.importFileVDI(ctx, vdiRef, path, info.Size(), "crabbox import ISO", "Import Crabbox installer ISO"); err != nil {
		drive.DestroyVDI = true
		return c.rollbackConfigDrive(ctx, drive, err)
	}
	if req.MarkReadOnly {
		if _, err := c.call(ctx, "VDI.set_read_only", c.session, vdiRef, true); err != nil {
			drive.DestroyVDI = true
			return c.rollbackConfigDrive(ctx, drive, err)
		}
	}
	return drive, nil
}

func (c *xapiClient) AttachDisk(ctx context.Context, req xcpNgDiskAttachRequest) (xcpNgConfigDrive, error) {
	if req.VMRef == "" {
		return xcpNgConfigDrive{}, exit(3, "xcp-ng VM ref is required for disk attachment")
	}
	if req.SRRef == "" {
		return xcpNgConfigDrive{}, exit(3, "xcp-ng sr or sr UUID is required for disk attachment")
	}
	sizeBytes := req.SizeBytes
	if sizeBytes <= 0 {
		sizeBytes = 20 * 1024 * 1024 * 1024
	}
	labels := vmDiskLabels(req.Labels)
	name := firstNonBlank(strings.TrimSpace(req.Name), "crabbox-iso-install-disk")
	description := firstNonBlank(strings.TrimSpace(req.Description), "Crabbox ISO install disk")
	vdiRef, err := c.callString(ctx, "VDI.create", c.session, map[string]any{
		"name_label":       name,
		"name_description": description,
		"SR":               req.SRRef.value(),
		"virtual_size":     sizeBytes,
		"type":             "user",
		"sharable":         false,
		"read_only":        false,
		"xenstore_data":    map[string]string{},
		"sm_config":        map[string]string{"type": "raw"},
		"tags":             []string{},
		"other_config":     labels,
	})
	if err != nil {
		return xcpNgConfigDrive{}, err
	}
	drive := xcpNgConfigDrive{VDIRef: vdiRef, Name: name, Labels: labels, DestroyVDI: req.DestroyVDI}
	vbdRef, err := c.callString(ctx, "VBD.create", c.session, map[string]any{
		"VM":                       req.VMRef.value(),
		"VDI":                      vdiRef,
		"userdevice":               firstNonBlank(req.UserDevice, "0"),
		"bootable":                 true,
		"mode":                     "RW",
		"type":                     "Disk",
		"empty":                    false,
		"unpluggable":              req.Unpluggable,
		"qos_algorithm_type":       "",
		"qos_algorithm_params":     map[string]string{},
		"qos_supported_algorithms": []string{},
		"other_config":             labels,
	})
	if err != nil {
		drive.DestroyVDI = true
		return c.rollbackConfigDrive(ctx, drive, err)
	}
	drive.VBDRef = vbdRef
	return drive, nil
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
		"sm_config":        map[string]string{"type": "raw"},
		"tags":             []string{},
		"other_config":     labels,
	})
	if err != nil {
		return xcpNgConfigDrive{}, err
	}
	drive := xcpNgConfigDrive{VDIRef: vdiRef, Name: name, Labels: labels, DestroyVDI: true}
	if err := c.importRawVDI(ctx, vdiRef, image); err != nil {
		return c.rollbackConfigDrive(ctx, drive, err)
	}
	vbdRef, err := c.callString(ctx, "VBD.create", c.session, map[string]any{
		"VM":                       req.VMRef.value(),
		"VDI":                      vdiRef,
		"userdevice":               "autodetect",
		"bootable":                 false,
		"mode":                     "RW",
		"type":                     "Disk",
		"empty":                    false,
		"unpluggable":              true,
		"qos_algorithm_type":       "",
		"qos_algorithm_params":     map[string]string{},
		"qos_supported_algorithms": []string{},
		"other_config":             labels,
	})
	if err != nil {
		return c.rollbackConfigDrive(ctx, drive, err)
	}
	drive.VBDRef = vbdRef
	drive.DestroyVDI = true
	return drive, nil
}

func (c *xapiClient) AttachISO(ctx context.Context, req xcpNgISOAttachRequest) (xcpNgConfigDrive, error) {
	labels := isoMediaLabels(req.Labels)
	vdiRef := req.ISO.VDIRef
	if req.Empty {
		vdiRef = "OpaqueRef:NULL"
	}
	vbdRef, err := c.callString(ctx, "VBD.create", c.session, map[string]any{
		"VM":                       req.VMRef.value(),
		"VDI":                      vdiRef,
		"userdevice":               firstNonBlank(req.UserDevice, "3"),
		"bootable":                 req.Bootable,
		"mode":                     "RO",
		"type":                     "CD",
		"empty":                    req.Empty,
		"unpluggable":              req.Unpluggable,
		"qos_algorithm_type":       "",
		"qos_algorithm_params":     map[string]string{},
		"qos_supported_algorithms": []string{},
		"other_config":             labels,
	})
	if err != nil {
		return xcpNgConfigDrive{}, err
	}
	return xcpNgConfigDrive{VDIRef: req.ISO.VDIRef, VBDRef: vbdRef, Name: req.ISO.NameLabel, Labels: labels, DestroyVDI: false}, nil
}

func (c *xapiClient) StartVM(ctx context.Context, ref xapiRef) error {
	if _, err := c.call(ctx, "VM.start", c.session, ref.value(), false, false); err != nil {
		return err
	}
	if err := c.waitForPowerState(ctx, ref.value(), "Running", xcpNgStartTimeout); err != nil {
		return err
	}
	return c.waitForDomID(ctx, ref.value(), xcpNgStartTimeout)
}

func (c *xapiClient) SetVMBootOrder(ctx context.Context, ref xapiRef, order string) error {
	order = strings.TrimSpace(order)
	if order == "" {
		return exit(3, "xcp-ng VM boot order is required")
	}
	if err := c.callDiscard(ctx, "VM.remove_from_HVM_boot_params", c.session, ref.value(), "order"); err != nil && !isMissingMapKey(err) {
		return err
	}
	_, err := c.call(ctx, "VM.add_to_HVM_boot_params", c.session, ref.value(), "order", order)
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
	return guestIPv4FromNetworks(xmlValueToStringMap(value), c.guestCIDR)
}

func guestIPv4FromNetworks(networkMap map[string]string, guestCIDR string) (string, error) {
	var guestNetwork *net.IPNet
	if guestCIDR = strings.TrimSpace(guestCIDR); guestCIDR != "" {
		parsedIP, parsedNetwork, err := net.ParseCIDR(guestCIDR)
		if err != nil || parsedIP.To4() == nil {
			return "", guestProbeConfigError{message: fmt.Sprintf("invalid CRABBOX_XCP_NG_GUEST_CIDR %q", guestCIDR)}
		}
		guestNetwork = parsedNetwork
	}
	keys := make([]string, 0, len(networkMap))
	for key := range networkMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	type candidate struct {
		ip      string
		primary bool
	}
	candidates := make([]candidate, 0, len(keys))
	candidateByIP := make(map[string]int, len(keys))
	for _, key := range keys {
		ip := usableIPv4(networkMap[key])
		if ip == "" || guestNetwork != nil && !guestNetwork.Contains(net.ParseIP(ip)) {
			continue
		}
		if index, ok := candidateByIP[ip]; ok {
			candidates[index].primary = candidates[index].primary || strings.HasPrefix(key, "0/")
			continue
		}
		candidateByIP[ip] = len(candidates)
		candidates = append(candidates, candidate{ip: ip, primary: strings.HasPrefix(key, "0/")})
	}
	if len(candidates) == 1 {
		return candidates[0].ip, nil
	}
	primary := make([]candidate, 0, 1)
	for _, candidate := range candidates {
		if candidate.primary {
			primary = append(primary, candidate)
		}
	}
	if len(primary) == 1 {
		return primary[0].ip, nil
	}
	if len(candidates) > 1 {
		return "", errors.New("multiple guest ipv4 addresses reported by XCP-ng guest metrics; set CRABBOX_XCP_NG_GUEST_CIDR to select one")
	}
	return "", errors.New("no guest ipv4 address reported by XCP-ng guest metrics")
}

func (c *xapiClient) DiscoverGuestIPv4(ctx context.Context, ref xapiRef) (string, error) {
	resolvedRef, err := c.vmRefForID(ctx, ref.value())
	if err != nil {
		return "", err
	}
	macs, err := c.vmVIFMACs(ctx, resolvedRef)
	if err != nil {
		return "", err
	}
	return discoverIPv4ByMAC(ctx, macs, c.guestCIDR)
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
	return xcpNgVMToServer(record, record.Labels, ""), nil
}

func (c *xapiClient) vmVIFMACs(ctx context.Context, vmRef string) ([]string, error) {
	value, err := c.call(ctx, "VM.get_VIFs", c.session, vmRef)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	macs := make([]string, 0, len(value.Array))
	for _, vifRef := range xmlValueToStrings(value) {
		mac, err := c.callString(ctx, "VIF.get_MAC", c.session, vifRef)
		if err != nil {
			return nil, err
		}
		mac = normalizeMAC(mac)
		if mac == "" {
			continue
		}
		if _, ok := seen[mac]; ok {
			continue
		}
		seen[mac] = struct{}{}
		macs = append(macs, mac)
	}
	return macs, nil
}

func discoverIPv4ByMAC(ctx context.Context, macs []string, guestCIDR string) (string, error) {
	macs = normalizeMACList(macs)
	if len(macs) == 0 {
		return "", errors.New("xcp-ng guest IP fallback requires at least one VIF MAC")
	}
	var guestNetwork *net.IPNet
	if strings.TrimSpace(guestCIDR) != "" {
		var err error
		guestNetwork, err = parseGuestProbeNetwork(guestCIDR)
		if err != nil {
			return "", err
		}
	}
	if ip, err := matchIPv4ByMAC(ctx, macs, guestNetwork); ip != "" || err != nil {
		return ip, err
	}
	networks, err := xcpNgLocalIPv4Networks()
	if err != nil {
		return "", err
	}
	networks, err = guestProbeNetworks(networks, guestCIDR)
	if err != nil {
		return "", err
	}
	if len(networks) == 0 {
		return "", errors.New("xcp-ng guest IP fallback found no local IPv4 networks to probe")
	}
	probeLocalNetworks(ctx, networks)
	return matchIPv4ByMAC(ctx, macs, guestNetwork)
}

func matchIPv4ByMAC(ctx context.Context, macs []string, guestNetwork *net.IPNet) (string, error) {
	table, err := xcpNgReadARPTable(ctx)
	if err != nil {
		return "", err
	}
	for _, mac := range macs {
		if ip, ok := table[mac]; ok {
			ip = usableIPv4(ip)
			if ip != "" && (guestNetwork == nil || guestNetwork.Contains(net.ParseIP(ip))) {
				return ip, nil
			}
		}
	}
	return "", nil
}

func guestProbeNetworks(networks []net.IPNet, guestCIDR string) ([]net.IPNet, error) {
	guestNetwork, err := parseGuestProbeNetwork(guestCIDR)
	if err != nil {
		return nil, err
	}
	first := guestNetwork.IP.To4()
	last := ipv4FromUint32(binaryIPv4(first) | ^binaryIPv4(net.IP(guestNetwork.Mask)))
	for _, localNetwork := range networks {
		if localNetwork.Contains(first) && localNetwork.Contains(last) {
			return []net.IPNet{*guestNetwork}, nil
		}
	}
	return nil, guestProbeConfigError{message: fmt.Sprintf("CRABBOX_XCP_NG_GUEST_CIDR %q is not attached to a local interface", strings.TrimSpace(guestCIDR))}
}

func parseGuestProbeNetwork(guestCIDR string) (*net.IPNet, error) {
	guestCIDR = strings.TrimSpace(guestCIDR)
	if guestCIDR == "" {
		return nil, errors.New("xcp-ng active guest IP discovery is disabled; set CRABBOX_XCP_NG_GUEST_CIDR to opt in")
	}
	parsedIP, guestNetwork, err := net.ParseCIDR(guestCIDR)
	if err != nil || parsedIP.To4() == nil {
		return nil, guestProbeConfigError{message: fmt.Sprintf("invalid CRABBOX_XCP_NG_GUEST_CIDR %q", guestCIDR)}
	}
	ones, bits := guestNetwork.Mask.Size()
	if bits != 32 || ones < 24 {
		return nil, guestProbeConfigError{message: "CRABBOX_XCP_NG_GUEST_CIDR must be an IPv4 /24 or narrower range"}
	}
	return guestNetwork, nil
}

func probeLocalNetworks(ctx context.Context, networks []net.IPNet) {
	const (
		concurrency = 32
		timeout     = 200 * time.Millisecond
	)
	ports := []string{"22", "2222"}
	if len(ports) == 0 {
		return
	}
	port := ports[0]
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, network := range networks {
		for _, host := range enumerateIPv4Hosts(network) {
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(host, port string) {
				defer wg.Done()
				defer func() { <-sem }()
				_ = xcpNgProbeTCPAddress(ctx, net.JoinHostPort(host, port), timeout)
			}(host, port)
		}
		if ctx.Err() != nil {
			break
		}
	}
	wg.Wait()
}

func localIPv4Networks() ([]net.IPNet, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	out := make([]net.IPNet, 0, len(ifaces))
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			clamped, ok := clampIPv4Network(*ipNet)
			if !ok {
				continue
			}
			key := clamped.String()
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, clamped)
		}
	}
	return out, nil
}

func clampIPv4Network(network net.IPNet) (net.IPNet, bool) {
	ip := network.IP.To4()
	if ip == nil {
		return net.IPNet{}, false
	}
	_, bits := network.Mask.Size()
	if bits != net.IPv4len*8 {
		return net.IPNet{}, false
	}
	network.IP = ip.Mask(network.Mask)
	return network, true
}

func enumerateIPv4Hosts(network net.IPNet) []string {
	ip := network.IP.To4()
	if ip == nil {
		return nil
	}
	ones, bits := network.Mask.Size()
	if bits != net.IPv4len*8 {
		return nil
	}
	hostBits := bits - ones
	if hostBits == 0 {
		return []string{ip.String()}
	}
	total := 1 << hostBits
	if total > 256 {
		total = 256
	}
	base := binaryIPv4(ip)
	firstOffset, lastOffset := 1, total-1
	if hostBits == 1 {
		firstOffset, lastOffset = 0, total
	}
	hosts := make([]string, 0, lastOffset-firstOffset)
	for offset := firstOffset; offset < lastOffset; offset++ {
		hosts = append(hosts, ipv4FromUint32(base+uint32(offset)).String())
	}
	return hosts
}

func readARPTable(ctx context.Context) (map[string]string, error) {
	table := map[string]string{}
	var firstErr error
	var succeeded bool
	for _, spec := range [][]string{{"arp", "-an"}, {"ip", "neigh"}} {
		out, err := xcpNgRunNeighborCommand(ctx, spec[0], spec[1:]...)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		succeeded = true
		mergeARPTable(table, string(out))
	}
	if len(table) > 0 || succeeded {
		return table, nil
	}
	return nil, firstErr
}

func mergeARPTable(table map[string]string, output string) {
	arpPattern := regexp.MustCompile(`\((\d+\.\d+\.\d+\.\d+)\)\s+at\s+([0-9a-fA-F:]{17})`)
	for _, match := range arpPattern.FindAllStringSubmatch(output, -1) {
		ip := usableIPv4(match[1])
		mac := normalizeMAC(match[2])
		if ip == "" || mac == "" {
			continue
		}
		table[mac] = ip
	}
	ipNeighPattern := regexp.MustCompile(`(?m)^(\d+\.\d+\.\d+\.\d+)\s+\S+\s+\S+\s+lladdr\s+([0-9a-fA-F:]{17})\b`)
	for _, match := range ipNeighPattern.FindAllStringSubmatch(output, -1) {
		ip := usableIPv4(match[1])
		mac := normalizeMAC(match[2])
		if ip == "" || mac == "" {
			continue
		}
		table[mac] = ip
	}
}

func normalizeMACList(macs []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(macs))
	for _, mac := range macs {
		mac = normalizeMAC(mac)
		if mac == "" {
			continue
		}
		if _, ok := seen[mac]; ok {
			continue
		}
		seen[mac] = struct{}{}
		out = append(out, mac)
	}
	return out
}

func normalizeMAC(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", ":")
	if len(value) != 17 {
		return ""
	}
	for i, ch := range value {
		if i%3 == 2 {
			if ch != ':' {
				return ""
			}
			continue
		}
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return ""
		}
	}
	return value
}

func binaryIPv4(ip net.IP) uint32 {
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func ipv4FromUint32(value uint32) net.IP {
	return net.IPv4(byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
}

func (c *xapiClient) SetLabels(ctx context.Context, id string, labels map[string]string) error {
	ref, err := c.vmRefForID(ctx, id)
	if err != nil {
		return err
	}
	return c.setVMLabels(ctx, ref, labels)
}

func (c *xapiClient) DeleteServer(ctx context.Context, id string) error {
	ref, err := c.vmRefForID(ctx, id)
	if err != nil {
		return err
	}
	server, err := c.GetServer(ctx, ref)
	if err != nil {
		return err
	}
	if !isCrabboxLease(server) {
		return exit(4, "refusing to delete non-Crabbox xcp-ng VM: %s", id)
	}
	isTemplate, err := c.callString(ctx, "VM.get_is_a_template", c.session, ref)
	if err != nil {
		return fmt.Errorf("inspect xcp-ng VM template state before delete: %w", err)
	}
	return c.deleteServer(ctx, ref, isTemplate == "true")
}

func (c *xapiClient) DeleteFreshServer(ctx context.Context, id, vtpmRef string) error {
	return c.deleteServer(ctx, id, true, vtpmRef)
}

func (c *xapiClient) deleteServer(ctx context.Context, id string, forceDisks bool, vtpmRefs ...string) error {
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
	for _, vtpmRef := range vtpmRefs {
		if err := c.DeleteVTPM(ctx, vtpmRef); err != nil {
			return err
		}
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
		if strings.EqualFold(state, want) {
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

func (c *xapiClient) waitForDomID(ctx context.Context, ref string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		domid, err := c.callString(ctx, "VM.get_domid", c.session, ref)
		if err != nil {
			return err
		}
		domid = strings.TrimSpace(domid)
		if domid != "" && domid != "-1" {
			return nil
		}
		if time.Now().After(deadline) {
			return exit(5, "timed out waiting for xcp-ng VM %s domid assignment", ref)
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(xcpNgStartPollInterval):
		}
	}
}

func (c *xapiClient) DeleteConfigDrive(ctx context.Context, drive xcpNgConfigDrive) error {
	if drive.VBDRef != "" {
		if drive.Labels["resource"] == "installer-media" {
			if err := c.callDiscard(ctx, "VBD.eject", c.session, drive.VBDRef); err != nil && !isNotFound(err) && !isAlreadyDetached(err) && !isXAPIFault(err, "VBD_IS_EMPTY") && !isXAPIFault(err, "VBD_NOT_REMOVABLE_MEDIA") {
				return err
			}
		}
		if err := c.callDiscard(ctx, "VBD.unplug", c.session, drive.VBDRef); err != nil && !isNotFound(err) && !isAlreadyDetached(err) && !isNotUnpluggable(err) && !isXAPIHaltedPowerStateFault(err) {
			return err
		}
		if err := c.callDiscard(ctx, "VBD.destroy", c.session, drive.VBDRef); err != nil && !isNotFound(err) {
			return err
		}
	}
	if drive.DestroyVDI && drive.VDIRef != "" {
		if err := c.callDiscard(ctx, "VDI.destroy", c.session, drive.VDIRef); err != nil && !isNotFound(err) {
			return err
		}
	}
	return nil
}

func (c *xapiClient) DeleteVTPM(ctx context.Context, ref string) error {
	if strings.TrimSpace(ref) == "" {
		return nil
	}
	if err := c.callDiscard(ctx, "VTPM.destroy", c.session, ref); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
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

// redactURLUserinfo removes any URL userinfo from the input so url.Parse
// error messages and downstream diagnostics cannot echo the original
// credentials when the URL is otherwise malformed.
func redactURLUserinfo(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			return raw[:i+3] + rest[at+1:]
		}
		return raw
	}
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		return raw[at+1:]
	}
	return raw
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
		drive.DestroyVDI = true
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
		if !strings.EqualFold(xmlStructString(record, "type"), "Disk") {
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
		disks = append(disks, xcpNgConfigDrive{VDIRef: vdiRef, VBDRef: vbdRef, Name: xmlStructString(vdi, "name_label"), DestroyVDI: true})
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
	return c.importReaderVDI(ctx, vdiRef, bytes.NewReader(image), int64(len(image)), "crabbox import config drive", "Import Crabbox cloud-init config drive")
}

func (c *xapiClient) importFileVDI(ctx context.Context, vdiRef, path string, size int64, taskName, taskDescription string) error {
	file, err := os.Open(path)
	if err != nil {
		return exit(3, "open xcp-ng import file %s: %v", path, err)
	}
	defer file.Close()
	return c.importReaderVDI(ctx, vdiRef, file, size, taskName, taskDescription)
}

func (c *xapiClient) importReaderVDI(ctx context.Context, vdiRef string, reader io.Reader, size int64, taskName, taskDescription string) error {
	taskRef, err := c.callString(ctx, "task.create", c.session, firstNonBlank(taskName, "crabbox import config drive"), firstNonBlank(taskDescription, "Import Crabbox cloud-init config drive"))
	if err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := xcpNgRollbackContext(ctx)
		defer cancel()
		_ = c.callDiscard(cleanupCtx, "task.destroy", c.session, taskRef)
	}()

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
	uploadCtx, uploadCancel := context.WithTimeout(ctx, xcpNgLongRequestTimeout)
	defer uploadCancel()
	req, err := http.NewRequestWithContext(uploadCtx, http.MethodPut, u.String(), reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if size >= 0 {
		req.ContentLength = size
	}
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
	return c.waitForTaskSuccess(uploadCtx, taskRef)
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
		labels := parseRenderedLabels(xmlStructStringMap(record, "other_config")["crabbox:labels"])
		if xmlStructString(record, "is_a_template") == "true" && !isCrabboxLease(xcpNgVMToServer(xapiVM{Ref: ref}, labels, "")) {
			continue
		}
		vms = append(vms, xapiVM{
			Ref:        ref,
			UUID:       xmlStructString(record, "uuid"),
			Name:       xapiName(xmlStructString(record, "name_label")),
			PowerState: xmlStructString(record, "power_state"),
			Labels:     labels,
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
	otherConfig, err := c.callStringMap(ctx, "VM.get_other_config", c.session, ref)
	if err != nil {
		return err
	}
	for key, value := range values {
		oldValue, hadOldValue := otherConfig[key]
		if err := c.callDiscard(ctx, "VM.remove_from_other_config", c.session, ref, key); err != nil && !isMissingMapKey(err) {
			return err
		}
		if _, err := c.call(ctx, "VM.add_to_other_config", c.session, ref, key, value); err != nil {
			if hadOldValue {
				restoreCtx, cancel := xcpNgRollbackContext(ctx)
				_, restoreErr := c.call(restoreCtx, "VM.add_to_other_config", c.session, ref, key, oldValue)
				cancel()
				if restoreErr != nil {
					return errors.Join(err, fmt.Errorf("restore xcp-ng VM metadata %q: %w", key, restoreErr))
				}
			}
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

func (c *xapiClient) callStringMap(ctx context.Context, method string, params ...any) (map[string]string, error) {
	value, err := c.call(ctx, method, params...)
	if err != nil {
		return nil, err
	}
	return xmlValueToStringMap(value), nil
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
	requestCtx, cancel := context.WithTimeout(ctx, xcpNgRequestTimeoutForMethod(method))
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.endpoint, bytes.NewReader(body))
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

func xcpNgRequestTimeoutForMethod(method string) time.Duration {
	switch method {
	case "VM.clone", "VM.copy", "VM.provision":
		return xcpNgLongRequestTimeout
	default:
		return xcpNgRequestTimeout
	}
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
		u, err = url.Parse(redactURLUserinfo(raw))
		if err != nil {
			return "", err
		}
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
		currentURL, err = url.Parse(redactURLUserinfo(current))
		if err != nil {
			return "", err
		}
	}
	currentURL.User = nil
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
	data, err := json.Marshal(labels)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func parseRenderedLabels(value string) map[string]string {
	labels := map[string]string{}
	if json.Unmarshal([]byte(value), &labels) == nil {
		return labels
	}
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
		fmt.Fprintf(b, "<string>%d</string>", v)
	case int64:
		fmt.Fprintf(b, "<string>%d</string>", v)
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

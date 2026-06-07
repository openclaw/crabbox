package xcpng

import "strings"

type xapiRef string

func (r xapiRef) value() string { return string(r) }

type xapiVM struct {
	Ref        string
	UUID       string
	Name       string
	PowerState string
	Labels     map[string]string
}

type xapiObject struct {
	Ref       string
	UUID      string
	NameLabel string
}

type xcpNgISOMediaRef struct {
	VDIRef    string
	UUID      string
	NameLabel string
	Source    string
}

type xcpNgISOAttachRequest struct {
	VMRef       xapiRef
	ISO         xcpNgISOMediaRef
	UserDevice  string
	Bootable    bool
	Empty       bool
	Labels      map[string]string
	Unpluggable bool
}

type xcpNgVIFSpec struct {
	Device     string
	NetworkRef xapiRef
	MAC        string
	MTU        int
	Labels     map[string]string
}

type xcpNgFreshVMRequest struct {
	Name        string
	Description string
	HostRef     xapiRef
	Network     *xcpNgVIFSpec
	MemoryBytes int64
	VCPUsMax    int
	VCPUsStart  int
	Labels      map[string]string
	Platform    map[string]string
	HVMBoot     map[string]string
	PVArgs      string
	DomainType  string
	SecureBoot  bool
	Affinity    xapiRef
}

type xcpNgFreshVMResult struct {
	VM     xapiVM
	VIFRef string
}

func xapiName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "unknown"
	}
	return name
}

package cli

import "errors"

const guestNetworkManagedCoordinatorUnsupported = "managed coordinator does not implement guest network policy enforcement/inspection protocol"

func rejectUnimplementedGuestNetworkCoordinator(cfg Config) error {
	if cfg.GuestNetworkPolicy == nil {
		return nil
	}
	return errors.New(guestNetworkManagedCoordinatorUnsupported)
}

const (
	GuestNetworkRouteDirect             = "direct"
	GuestNetworkRouteManagedCoordinator = "managed-coordinator"
)

// Discovery describes implemented allocation routes, never a live attestation.
// Controller identity acknowledgments do not change the allocation route.
type GuestNetworkCapabilityDescriptor struct {
	Version                    int      `json:"version"`
	AllocationRoutes           []string `json:"allocationRoutes"`
	ReceiptOutputs             []string `json:"receiptOutputs"`
	ControllerIdentityProtocol string   `json:"controllerIdentityProtocol"`
}
type GuestNetworkRouteCapability struct {
	Version         int    `json:"version,omitempty"`
	AllocationRoute string `json:"allocationRoute"`
	State           string `json:"state"`
	Reason          string `json:"reason,omitempty"`
}

func guestNetworkDiscovery(backend Backend) *GuestNetworkCapabilityDescriptor {
	capable, ok := backend.(GuestNetworkBackend)
	if !ok || capable.GuestNetworkVersion() != GuestNetworkVersion {
		return nil
	}
	return &GuestNetworkCapabilityDescriptor{Version: GuestNetworkVersion, AllocationRoutes: []string{GuestNetworkRouteDirect, GuestNetworkRouteManagedCoordinator}, ReceiptOutputs: []string{"warmup-timing", "inspect"}, ControllerIdentityProtocol: "lease-identity-v1"}
}
func guestNetworkRouteCapability(cfg Config, backend Backend, coordinator bool) GuestNetworkRouteCapability {
	route := GuestNetworkRouteDirect
	if coordinator {
		route = GuestNetworkRouteManagedCoordinator
	}
	result := GuestNetworkRouteCapability{AllocationRoute: route, State: "unsupported", Reason: "provider has no audited guest network enforcement/inspection"}
	capable, ok := backend.(GuestNetworkBackend)
	if !ok {
		return result
	}
	result.Version = capable.GuestNetworkVersion()
	if result.Version != GuestNetworkVersion {
		result.Reason = "unknown guest network capability version"
		return result
	}
	if coordinator {
		result.State = "server-qualification-required"
		result.Reason = "requires authenticated versioned server policy/enforcement handshake"
		return result
	}
	result.State = "implemented"
	result.Reason = ""
	return result
}
func guestNetworkCapabilityForConfig(cfg Config) *GuestNetworkRouteCapability {
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return nil
	}
	var backend Backend
	if source, ok := provider.(ProviderBackendCapabilitySource); ok {
		backend = source.BackendCapabilities()
	}
	if backend == nil {
		backend = guestNetworkMetadataBackend{spec: provider.Spec()}
	}
	result := guestNetworkRouteCapability(cfg, backend, ShouldUseCoordinator(cfg, provider.Spec()))
	return &result
}

type guestNetworkMetadataBackend struct{ spec ProviderSpec }

func (b guestNetworkMetadataBackend) Spec() ProviderSpec { return b.spec }

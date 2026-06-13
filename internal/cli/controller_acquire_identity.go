package cli

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	controllerAcquireIdentityAddressEnv = "CRABBOX_ADAPTER_ACQUIRE_IDENTITY_ADDRESS"
	controllerAcquireIdentityTokenEnv   = "CRABBOX_ADAPTER_ACQUIRE_IDENTITY_TOKEN"
	controllerAcquireIdentityMaxBytes   = 64 << 10
	controllerAcquireIdentityPreAuthTTL = time.Second
	controllerAcquireIdentityMaxPending = 64
)

type controllerAcquireIdentity struct {
	LeaseID    string `json:"leaseId"`
	Slug       string `json:"slug"`
	Provider   string `json:"provider"`
	ResourceID string `json:"resourceId"`
}

func controllerAcquireIdentityFromLease(provider string, lease LeaseTarget) controllerAcquireIdentity {
	return controllerAcquireIdentity{
		LeaseID:    lease.LeaseID,
		Slug:       serverSlug(lease.Server),
		Provider:   provider,
		ResourceID: strings.TrimSpace(lease.Server.CloudID),
	}
}

type controllerAcquireIdentityRequest struct {
	Token    string                    `json:"token"`
	Identity controllerAcquireIdentity `json:"identity"`
}

type controllerAcquireIdentityResponse struct {
	Accepted bool   `json:"accepted"`
	Error    string `json:"error,omitempty"`
}

type controllerAcquireIdentityResult struct {
	Identity controllerAcquireIdentity
	Err      error
}

type controllerAcquireIdentityGate struct {
	listener net.Listener
	token    string
	result   chan controllerAcquireIdentityResult
	done     chan struct{}
	once     sync.Once
	authMu   sync.Mutex
	authed   bool
}

func newControllerAcquireIdentityGate(onIdentity func(controllerAcquireIdentity) error) (*controllerAcquireIdentityGate, error) {
	if onIdentity == nil {
		return nil, fmt.Errorf("controller acquire identity callback is required")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for controller acquire identity: %w", err)
	}
	token, err := newWebVNCDaemonNonce()
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("create controller acquire identity token: %w", err)
	}
	gate := &controllerAcquireIdentityGate{
		listener: listener,
		token:    token,
		result:   make(chan controllerAcquireIdentityResult, 1),
		done:     make(chan struct{}),
	}
	go gate.serve(onIdentity)
	return gate, nil
}

func (g *controllerAcquireIdentityGate) environment() map[string]string {
	return map[string]string{
		controllerAcquireIdentityAddressEnv: g.listener.Addr().String(),
		controllerAcquireIdentityTokenEnv:   g.token,
	}
}

func (g *controllerAcquireIdentityGate) close() {
	g.once.Do(func() {
		_ = g.listener.Close()
	})
	<-g.done
}

func (g *controllerAcquireIdentityGate) wait() controllerAcquireIdentityResult {
	return <-g.result
}

func (g *controllerAcquireIdentityGate) serve(onIdentity func(controllerAcquireIdentity) error) {
	var handlers sync.WaitGroup
	pending := make(chan struct{}, controllerAcquireIdentityMaxPending)
	defer func() {
		handlers.Wait()
		close(g.done)
	}()
	for {
		conn, err := g.listener.Accept()
		if err != nil {
			// An authenticated handler owns the terminal result. The client can
			// receive its durable ACK and close the gate just before that handler
			// publishes, so the listener-close path must not win the result race.
			if !g.authenticationClaimed() {
				g.publish(controllerAcquireIdentityResult{Err: fmt.Errorf("controller warmup did not acknowledge a raw acquire identity: %w", err)})
			}
			return
		}
		select {
		case pending <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer func() { <-pending }()
			defer conn.Close()
			result, authenticated := g.handleConnection(conn, onIdentity)
			if !authenticated {
				return
			}
			g.publish(result)
			g.once.Do(func() { _ = g.listener.Close() })
		}()
	}
}

func (g *controllerAcquireIdentityGate) handleConnection(conn net.Conn, onIdentity func(controllerAcquireIdentity) error) (controllerAcquireIdentityResult, bool) {
	_ = conn.SetDeadline(time.Now().Add(controllerAcquireIdentityPreAuthTTL))
	decoder := json.NewDecoder(io.LimitReader(bufio.NewReader(conn), controllerAcquireIdentityMaxBytes))
	decoder.DisallowUnknownFields()
	var request controllerAcquireIdentityRequest
	if err := decoder.Decode(&request); err != nil {
		return controllerAcquireIdentityResult{}, false
	}
	if subtle.ConstantTimeCompare([]byte(request.Token), []byte(g.token)) != 1 {
		return controllerAcquireIdentityResult{}, false
	}
	if !g.claimAuthentication() {
		return controllerAcquireIdentityResult{}, false
	}
	// The short deadline protects only the unauthenticated JSON/token prefix.
	// Durable identity persistence may legitimately include a slow fsync.
	_ = conn.SetDeadline(time.Time{})
	result := controllerAcquireIdentityResult{Identity: request.Identity}
	if err := validateControllerAcquireIdentity(request.Identity); err != nil {
		result.Err = err
	} else if err := onIdentity(request.Identity); err != nil {
		result.Err = err
	}
	response := controllerAcquireIdentityResponse{Accepted: result.Err == nil}
	if result.Err != nil {
		response.Error = "controller did not durably accept provider identity"
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(response); err != nil && result.Err == nil {
		result.Err = fmt.Errorf("acknowledge controller acquire identity: %w", err)
	}
	return result, true
}

func (g *controllerAcquireIdentityGate) claimAuthentication() bool {
	g.authMu.Lock()
	defer g.authMu.Unlock()
	if g.authed {
		return false
	}
	g.authed = true
	return true
}

func (g *controllerAcquireIdentityGate) authenticationClaimed() bool {
	g.authMu.Lock()
	defer g.authMu.Unlock()
	return g.authed
}

func (g *controllerAcquireIdentityGate) publish(result controllerAcquireIdentityResult) {
	select {
	case g.result <- result:
	default:
	}
}

func acknowledgeControllerAcquireIdentity(ctx context.Context, identity controllerAcquireIdentity) error {
	address := strings.TrimSpace(os.Getenv(controllerAcquireIdentityAddressEnv))
	token := strings.TrimSpace(os.Getenv(controllerAcquireIdentityTokenEnv))
	if address == "" && token == "" {
		return nil
	}
	if address == "" || token == "" {
		return fmt.Errorf("controller acquire identity acknowledgment is incompletely configured")
	}
	if err := validateControllerAcquireIdentity(identity); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" {
		return fmt.Errorf("controller acquire identity address must be an IPv4 loopback listener")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", address)
	if err != nil {
		return fmt.Errorf("connect controller acquire identity gate: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	request := controllerAcquireIdentityRequest{Token: token, Identity: identity}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return fmt.Errorf("report controller acquire identity: %w", err)
	}
	var response controllerAcquireIdentityResponse
	decoder := json.NewDecoder(io.LimitReader(bufio.NewReader(conn), controllerAcquireIdentityMaxBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return fmt.Errorf("read controller acquire identity acknowledgment: %w", err)
	}
	if !response.Accepted {
		return errors.New(firstNonBlank(strings.TrimSpace(response.Error), "controller rejected provider identity"))
	}
	return nil
}

func controllerAcquireIdentityAcknowledgmentConfigured() bool {
	return strings.TrimSpace(os.Getenv(controllerAcquireIdentityAddressEnv)) != "" &&
		strings.TrimSpace(os.Getenv(controllerAcquireIdentityTokenEnv)) != ""
}

func validateControllerAcquireIdentity(identity controllerAcquireIdentity) error {
	if identity.LeaseID != strings.TrimSpace(identity.LeaseID) || !validLeaseClaimID(identity.LeaseID) {
		return fmt.Errorf("controller acquire identity has invalid lease ID")
	}
	if identity.Slug != strings.TrimSpace(identity.Slug) || normalizeLeaseSlug(identity.Slug) != identity.Slug {
		return fmt.Errorf("controller acquire identity has invalid slug")
	}
	for name, value := range map[string]string{
		"provider":    identity.Provider,
		"resource ID": identity.ResourceID,
	} {
		if value != strings.TrimSpace(value) || !validControllerInventoryIdentity(value) {
			return fmt.Errorf("controller acquire identity has invalid %s", name)
		}
	}
	return nil
}

func stripControllerAcquireIdentityEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, item := range env {
		name, _, ok := strings.Cut(item, "=")
		if ok && (name == controllerAcquireIdentityAddressEnv || name == controllerAcquireIdentityTokenEnv || name == controllerProcessTreeOwnedEnv) {
			continue
		}
		out = append(out, item)
	}
	return out
}

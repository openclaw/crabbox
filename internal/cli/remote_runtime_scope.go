package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openclaw/crabbox/internal/remoteruntime"
	"github.com/openclaw/crabbox/internal/runtimeartifact"
)

type nativeRuntimeScopeKey struct{}
type nativeRuntimeLeaseKey struct{}
type nativeRuntimeAdmissionKey struct{}

type nativeRuntimeAdmission struct{ lease string }
type nativeRuntimeTransportProbeKey struct{}

var errNativeRuntimeUnprepared = errors.New("native runtime is not prepared for this execution route")

type nativeRuntimeInstallKey struct {
	lease string
	route [32]byte
}

type nativeRuntimeInstall struct {
	done    chan struct{}
	runtime *remoteNativeRuntime
	err     error
	retain  error
}

// The run lifecycle, not an individual command, owns an installed runtime.
// Lease disposition must be recorded before access can be returned or revoked.
type nativeRuntimeScope struct {
	manifest        string
	controller      string
	discoveryErr    error
	loadOnce        sync.Once
	set             *runtimeartifact.LocalSet
	loadErr         error
	install         func(context.Context, SSHTarget, *runtimeartifact.LocalSet) (*remoteNativeRuntime, error)
	remove          func(context.Context, *remoteNativeRuntime) error
	ready           func(context.Context, *SSHTarget, io.Writer) error
	mu              sync.Mutex
	entries         map[nativeRuntimeInstallKey]*nativeRuntimeInstall
	leases          map[string]bool
	legacyBashReady map[nativeRuntimeInstallKey]bool
}

func newNativeRuntimeScope() *nativeRuntimeScope {
	controller, err := os.Executable()
	return newNativeRuntimeScopeForExecutable(controller, strings.TrimSpace(os.Getenv(runtimeArtifactsEnv)), err)
}

func newNativeRuntimeScopeForExecutable(controller, override string, discoveryErr error) *nativeRuntimeScope {
	manifest := ""
	if discoveryErr == nil {
		controller, manifest, discoveryErr = runtimeartifact.Discover(controller, override)
	}
	return &nativeRuntimeScope{
		manifest:     manifest,
		controller:   controller,
		discoveryErr: discoveryErr,
		install:      prepareNativeRuntime,
		remove:       func(ctx context.Context, runtime *remoteNativeRuntime) error { return runtime.close(ctx) },
		ready: func(ctx context.Context, target *SSHTarget, stderr io.Writer) error {
			probeCtx := context.WithValue(ctx, nativeRuntimeTransportProbeKey{}, true)
			return waitForSSHReady(probeCtx, target, stderr, "native runtime transport", 2*time.Minute)
		},
		entries: make(map[nativeRuntimeInstallKey]*nativeRuntimeInstall),
		leases:  make(map[string]bool),
	}
}

func nativeRuntimeLease(ctx context.Context) string {
	lease, _ := ctx.Value(nativeRuntimeLeaseKey{}).(string)
	return lease
}

func nativeRuntimeRouteKey(target SSHTarget) [32]byte {
	// Endpoint discovery and connection reuse do not change execution identity.
	target.FallbackPorts, target.NoControlMaster = nil, false
	target.preparedEndpoint = ""
	h := sha256.New()
	encoded, _ := json.Marshal(target)
	h.Write(encoded)
	// ChildEnv is deliberately excluded from SSHTarget serialization. Hash it
	// directly and never expose these transport-only values in diagnostics.
	keys := make([]string, 0, len(target.ChildEnv))
	for key := range target.ChildEnv {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		for _, value := range []string{key, target.ChildEnv[key]} {
			io.WriteString(h, strconv.Itoa(len(value))+":"+value)
		}
	}
	return [32]byte(h.Sum(nil))
}

func (s *nativeRuntimeScope) ensure(ctx context.Context, target SSHTarget) (*remoteNativeRuntime, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	key := nativeRuntimeInstallKey{nativeRuntimeLease(ctx), nativeRuntimeRouteKey(target)}
	s.mu.Lock()
	if s.leases[key.lease] {
		s.mu.Unlock()
		return nil, errors.New("native runtime lease is finalized")
	}
	entry := s.entries[key]
	first := entry == nil
	if first {
		entry = &nativeRuntimeInstall{done: make(chan struct{})}
		s.entries[key] = entry
	}
	s.mu.Unlock()
	if first {
		s.load(ctx)
		entry.err = s.loadErr
		if entry.err == nil {
			entry.err = context.Cause(ctx)
		}
		if entry.err == nil {
			target.ChildEnv = maps.Clone(target.ChildEnv)
			target.ChildEnvDenylist = slices.Clone(target.ChildEnvDenylist)
			target.FallbackPorts = slices.Clone(target.FallbackPorts)
			entry.runtime, entry.err = s.install(ctx, target, s.set)
			if entry.err == nil && entry.runtime == nil {
				entry.err = errors.New("native runtime installation returned no handle")
			}
		}
		close(entry.done)
	}
	select {
	case <-entry.done:
		return entry.runtime, entry.err
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

func (s *nativeRuntimeScope) load(ctx context.Context) {
	s.loadOnce.Do(func() {
		if s.discoveryErr != nil {
			s.loadErr = s.discoveryErr
			return
		}
		if s.manifest == "" {
			s.loadErr = errors.New(missingNativeRuntimePackDiagnostic)
			return
		}
		s.set, s.loadErr = runtimeartifact.OpenLocalSet(ctx, s.manifest, s.controller, remoteruntime.Protocol)
	})
}

func (s *nativeRuntimeScope) selected() bool { return s.manifest != "" || s.discoveryErr != nil }

// validateLocal checks lifecycle and local pack state before any guest access.
func (s *nativeRuntimeScope) validateLocal(ctx context.Context, target SSHTarget) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if _, err := s.prepared(ctx, target); err != nil && !errors.Is(err, errNativeRuntimeUnprepared) {
		return err
	}
	s.load(ctx)
	if s.loadErr != nil {
		return s.loadErr
	}
	return context.Cause(ctx)
}

// Admission belongs to the command operation after guest bootstrap. Transport
// builders only borrow the completed installation; they never initialize it.
func (s *nativeRuntimeScope) admit(ctx context.Context, target SSHTarget) (context.Context, error) {
	if !s.selected() || !isWindowsWSL2Target(target) {
		return ctx, nil
	}
	if ctx.Value(nativeRuntimeScopeKey{}) != s {
		return ctx, errors.New("native runtime admission requires its owning operation scope")
	}
	if _, err := s.ensure(ctx, target); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, nativeRuntimeAdmissionKey{}, &nativeRuntimeAdmission{lease: nativeRuntimeLease(ctx)}), nil
}

func contextWithoutNativeRuntimeAdmission(ctx context.Context) context.Context {
	return context.WithValue(ctx, nativeRuntimeAdmissionKey{}, (*nativeRuntimeAdmission)(nil))
}

func (s *nativeRuntimeScope) prepareCommandRuntime(ctx context.Context, target *SSHTarget, stderr io.Writer) (context.Context, error) {
	if !s.selected() || !isWindowsWSL2Target(*target) {
		return ctx, nil
	}
	if err := s.validateLocal(ctx, *target); err != nil {
		return ctx, err
	}
	// Resolve transport before reusing an installation: a previously
	// admitted port may disappear while another advertised port stays healthy.
	target.NoControlMaster = true
	if err := s.ready(ctx, target, stderr); err != nil {
		return ctx, err
	}
	if err := context.Cause(ctx); err != nil {
		return ctx, err
	}
	return s.admit(ctx, *target)
}

func admittedNativeRuntime(ctx context.Context, target SSHTarget) (*remoteNativeRuntime, error) {
	admission, selected := ctx.Value(nativeRuntimeAdmissionKey{}).(*nativeRuntimeAdmission)
	if !selected || admission == nil || !isWindowsWSL2Target(target) {
		return nil, nil
	}
	lease := nativeRuntimeLease(ctx)
	if admission.lease != lease {
		return nil, errors.New("native runtime admission belongs to another lease")
	}
	scope, ok := ctx.Value(nativeRuntimeScopeKey{}).(*nativeRuntimeScope)
	if !ok {
		return nil, errors.New("native runtime operation scope is unavailable")
	}
	return scope.prepared(ctx, target)
}

func (s *nativeRuntimeScope) prepared(ctx context.Context, target SSHTarget) (*remoteNativeRuntime, error) {
	lease := nativeRuntimeLease(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leases[lease] {
		return nil, errors.New("native runtime lease is finalized")
	}
	entry := s.entries[nativeRuntimeInstallKey{lease, nativeRuntimeRouteKey(target)}]
	if entry != nil {
		select {
		case <-entry.done:
			return entry.runtime, entry.err
		default:
		}
	}
	return nil, errNativeRuntimeUnprepared
}

func (s *nativeRuntimeScope) retain(runtime *remoteNativeRuntime, reason error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range s.entries {
		select {
		case <-entry.done:
			if entry.runtime == runtime {
				entry.retain = errors.Join(entry.retain, reason)
			}
		default:
		}
	}
}

// finalizeLease seals admission before cleanup. A terminal handoff makes no
// claim that files were removed; an unconfirmed release prohibits guest writes.
func (s *nativeRuntimeScope) finalizeLease(ctx context.Context, lease string, handoff bool, retain error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leases[lease] {
		return nil
	}
	var result error
	for key, entry := range s.entries {
		if key.lease != lease {
			continue
		}
		select {
		case <-entry.done:
		case <-ctx.Done():
			result = errors.Join(result, context.Cause(ctx))
			continue
		}
		if entry.runtime == nil {
			continue
		}
		if handoff {
			if entry.retain != nil {
				result = errors.Join(result, fmt.Errorf("native runtime at %s handed to lease teardown; file removal not verified: %w", entry.runtime.path, entry.retain))
			}
			continue
		}
		if reason := errors.Join(retain, entry.retain); reason != nil {
			result = errors.Join(result, fmt.Errorf("native runtime retained at %s: %w", entry.runtime.path, reason))
			continue
		}
		result = errors.Join(result, s.remove(ctx, entry.runtime))
	}
	s.leases[lease] = true
	return result
}

func (s *nativeRuntimeScope) finish(ctx context.Context) error {
	s.mu.Lock()
	leases := make(map[string]bool)
	for key := range s.entries {
		leases[key.lease] = true
	}
	s.mu.Unlock()
	var result error
	for lease := range leases {
		result = errors.Join(result, s.finalizeLease(ctx, lease, false, nil))
	}
	return result
}

func (s *nativeRuntimeScope) afterRelease(ctx context.Context, lease string, backend SSHLeaseBackend, outcome ReleaseLeaseOutcome, releaseErr error) error {
	policy, ok := backend.(ReleaseLeaseWorkspacePolicy)
	preserves := ok && policy.PreservesSSHWorkspaceAfterRelease()
	if releaseErr == nil && preserves {
		// A successful static release need not be terminal. Its captured route
		// remains usable until the late workspace-owner finalizer runs.
		return nil
	}
	if outcome.Terminal && !preserves {
		return s.finalizeLease(ctx, lease, true, nil)
	}
	return s.finalizeLease(ctx, lease, false, errors.Join(errors.New("lease release was not confirmed; no further runtime cleanup attempted"), releaseErr))
}

func (s *nativeRuntimeScope) hasLease(lease string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leases[lease] {
		return false
	}
	for key := range s.entries {
		if key.lease == lease {
			return true
		}
	}
	return false
}

// A standalone preflight owns a temporary scope; a run borrows its existing
// operation scope. Both retain the same executable when retirement is unknown.
type nativePreflightRuntime struct {
	scope      *nativeRuntimeScope
	shared     bool
	installed  *remoteNativeRuntime
	dispatched bool
}

func newNativePreflightRuntime(ctx context.Context) (context.Context, *nativePreflightRuntime) {
	scope, shared := ctx.Value(nativeRuntimeScopeKey{}).(*nativeRuntimeScope)
	if !shared {
		scope = newNativeRuntimeScope()
		ctx = context.WithValue(ctx, nativeRuntimeScopeKey{}, scope)
	}
	return ctx, &nativePreflightRuntime{scope: scope, shared: shared}
}

func (p *nativePreflightRuntime) prepare(ctx context.Context, target SSHTarget) (context.Context, error) {
	var err error
	p.installed, err = p.scope.ensure(ctx, target)
	if err != nil {
		return ctx, err
	}
	if isWindowsWSL2Target(target) {
		ctx, err = p.scope.admit(ctx, target)
		if err != nil {
			return ctx, err
		}
	}
	return context.WithValue(ctx, nativeRuntimeContextKey{}, p.installed), nil
}

func (p *nativePreflightRuntime) finish(ctx context.Context, nonce string, retired bool) error {
	if p.installed != nil && p.dispatched && !retired {
		p.scope.retain(p.installed, fmt.Errorf("unretired command stage /tmp/crabbox-command-%s", nonce))
	}
	if !p.shared {
		return p.scope.finish(context.WithoutCancel(ctx))
	}
	return nil
}

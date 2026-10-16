package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

type serveRelayControlsKey struct{}
type serveRelayControlBypassKey struct{}

type serveRelayControl struct {
	leaseID string
	proxy   string
	path    string
	close   func() error
}

// An invocation owns its masters, including when the lease is kept. The outer
// master carries one relay session; workload and owner use separate inner SSH
// connections so a workload cannot hold up owner renewal or release.
type serveRelayControls struct {
	mu      sync.Mutex
	entries map[[32]byte]serveRelayControl
	retired map[string]bool
	closed  bool
}

func serveRelayControlsFromContext(ctx context.Context) *serveRelayControls {
	controls, _ := ctx.Value(serveRelayControlsKey{}).(*serveRelayControls)
	return controls
}

func serveRelayControlKey(target SSHTarget) [32]byte {
	return sha256.Sum256([]byte(strings.Join([]string{
		target.Host, target.User, target.Port, target.Key, target.SSHHostKey,
		target.KnownHostsFile, target.HostKeyAlias, target.ProxyCommand,
		target.SSHJump.HostKey, target.ChildEnv[serveRelayAuthEnv],
	}, "\x00")))
}

func prepareServeRelayControl(ctx context.Context, target SSHTarget) (SSHTarget, error) {
	controls := serveRelayControlsFromContext(ctx)
	if controls == nil || !usesServeRelay(target) || runtime.GOOS == "windows" ||
		ctx.Value(serveRelayControlBypassKey{}) == true || target.ownerControlPath != "" {
		return target, nil
	}
	if target.DisableHostKeyChecking || target.Key == "" || target.KnownHostsFile == "" ||
		target.SSHHostKey == "" || target.SSHJump.HostKeyAlias == "" || !IsCanonicalLeaseID(target.Host) {
		return target, fmt.Errorf("serve relay reuse requires pinned lease and front-door identities")
	}
	key := serveRelayControlKey(target)
	controls.mu.Lock()
	defer controls.mu.Unlock()
	if controls.closed || controls.retired[target.Host] {
		return target, fmt.Errorf("serve relay invocation has closed the lease connection")
	}
	if entry, ok := controls.entries[key]; ok {
		target.ProxyCommand, target.ownerControlPath = entry.proxy, entry.path
		return target, nil
	}
	proxy, err := prepareServeRelayProxy(&target)
	if err != nil {
		return target, err
	}
	jump := target.SSHJump
	outer := SSHTarget{
		Host: jump.Host, Port: strconv.Itoa(jump.Port), User: "crabbox",
		Key: target.Key, KnownHostsFile: target.KnownHostsFile,
		SSHHostKey: jump.HostKey, HostKeyAlias: jump.HostKeyAlias,
		FallbackPorts: []string{}, ProxyCommand: proxy,
		ChildEnv: target.ChildEnv, ChildEnvDenylist: target.ChildEnvDenylist,
		DiagnosticSecrets: target.DiagnosticSecrets,
	}
	outer, closeOuter, err := startSSHControlMaster(ctx, outer, "60", "1")
	if err != nil {
		return target, err
	}
	if outer.ownerControlPath == "" {
		return target, errors.Join(fmt.Errorf("serve relay front-door master did not start"), closeOuter())
	}
	proxy, err = sshJumpProxyCommand(&target, outer.ownerControlPath)
	if err != nil {
		return target, errors.Join(err, closeOuter())
	}
	target.ProxyCommand = proxy
	inner, closeInner, err := startSSHControlMaster(ctx, target, "60", "1")
	if err != nil {
		return target, errors.Join(err, closeOuter())
	}
	if inner.ownerControlPath == "" {
		return target, errors.Join(fmt.Errorf("serve relay lease master did not start"), closeInner(), closeOuter())
	}
	if controls.entries == nil {
		controls.entries = make(map[[32]byte]serveRelayControl)
	}
	controls.entries[key] = serveRelayControl{
		leaseID: target.Host, proxy: proxy, path: inner.ownerControlPath,
		close: func() error { return errors.Join(closeInner(), closeOuter()) },
	}
	target.ownerControlPath = inner.ownerControlPath
	return target, nil
}

func (c *serveRelayControls) closeLease(leaseID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if leaseID == "" {
		c.closed = true
	} else {
		if c.retired == nil {
			c.retired = make(map[string]bool)
		}
		c.retired[leaseID] = true
	}
	var err error
	for key, entry := range c.entries {
		if leaseID == "" || entry.leaseID == leaseID {
			err = errors.Join(err, entry.close())
			delete(c.entries, key)
		}
	}
	return err
}

func closeServeRelayControls(ctx context.Context, leaseID string) error {
	if controls := serveRelayControlsFromContext(ctx); controls != nil {
		return controls.closeLease(leaseID)
	}
	return nil
}

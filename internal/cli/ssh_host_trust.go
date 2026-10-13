package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	xssh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func prepareLeaseSSHTrust(target *SSHTarget, leaseID string) error {
	if target == nil || strings.TrimSpace(target.SSHHostKey) == "" {
		return nil
	}
	if target.DisableHostKeyChecking {
		return Exit(2, "refusing authoritative SSH host key with host-key checking disabled")
	}
	leaseDir, err := ensureTestboxLeaseDirectory(leaseID)
	if err != nil {
		return err
	}
	expectedKnownHosts := filepath.Join(leaseDir, "known_hosts")
	if target.KnownHostsFile == "" {
		target.KnownHostsFile = expectedKnownHosts
	} else if filepath.Clean(target.KnownHostsFile) != expectedKnownHosts {
		return Exit(2, "authoritative coordinator SSH host key requires the isolated per-lease known_hosts path")
	}
	if target.HostKeyAlias == "" {
		alias, err := leaseHostKeyAlias(leaseID)
		if err != nil {
			return err
		}
		target.HostKeyAlias = alias
	}
	if target.SSHJump != nil {
		jump := *target.SSHJump
		target.SSHJump = &jump
		alias, err := leaseHostKeyAlias(leaseID)
		if err != nil {
			return err
		}
		jump.HostKeyAlias = strings.Replace(alias, "crabbox-lease-", "crabbox-serve-", 1)
		if err := prepareSSHJump(target); err != nil {
			return err
		}
	}
	return installAuthoritativeSSHHostKey(target)
}

func leaseHostKeyAlias(leaseID string) (string, error) {
	if !validLeaseClaimPathID(leaseID) {
		return "", invalidLeaseClaimIDError{id: leaseID}
	}
	sum := sha256.Sum256([]byte(leaseID))
	return "crabbox-lease-" + hex.EncodeToString(sum[:16]), nil
}

func installAuthoritativeSSHHostKey(target *SSHTarget) error {
	publicKey, err := parseAuthoritativeSSHHostKey(target.SSHHostKey)
	if err != nil {
		return err
	}
	token, err := authoritativeKnownHostToken(*target)
	if err != nil {
		return err
	}
	path := filepath.Clean(target.KnownHostsFile)
	if !filepath.IsAbs(path) {
		return Exit(2, "authoritative SSH host key requires an absolute isolated known_hosts path")
	}
	entry := knownhosts.Line([]string{token}, publicKey) + "\n"
	if jump := target.SSHJump; jump != nil {
		key, err := parseAuthoritativeSSHHostKey(jump.HostKey)
		if err != nil {
			return err
		}
		token := jump.HostKeyAlias
		if token == "" {
			token = net.JoinHostPort(jump.Host, strconv.Itoa(jump.Port))
		}
		entry += knownhosts.Line([]string{token}, key) + "\n"
	}
	if err := writeAuthoritativeKnownHosts(path, []byte(entry)); err != nil {
		return Exit(2, "install authoritative SSH host key in %s: %v", path, err)
	}
	return nil
}

func parseAuthoritativeSSHHostKey(value string) (xssh.PublicKey, error) {
	value = strings.TrimSpace(value)
	key, _, options, rest, err := xssh.ParseAuthorizedKey([]byte(value + "\n"))
	if err != nil || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, Exit(2, "coordinator-provided SSH host key is malformed; refresh or replace the lease")
	}
	switch key.Type() {
	case xssh.KeyAlgoED25519,
		xssh.KeyAlgoECDSA256,
		xssh.KeyAlgoECDSA384,
		xssh.KeyAlgoECDSA521,
		xssh.KeyAlgoSKED25519,
		xssh.KeyAlgoSKECDSA256,
		xssh.KeyAlgoRSA:
	default:
		return nil, Exit(2, "coordinator-provided SSH host key uses an unsupported algorithm; refresh or replace the lease")
	}
	return key, nil
}

func authoritativeKnownHostToken(target SSHTarget) (string, error) {
	if alias := strings.TrimSpace(target.HostKeyAlias); alias != "" {
		if alias != target.HostKeyAlias || strings.ContainsAny(alias, " ,\t\r\n\x00") {
			return "", Exit(2, "authoritative SSH host-key alias contains unsupported characters")
		}
		return knownhosts.Normalize(alias), nil
	}
	host := strings.TrimSpace(target.Host)
	port := strings.TrimSpace(target.Port)
	if host == "" || port == "" || host != target.Host || port != target.Port || strings.ContainsAny(host, "\r\n\x00") || strings.ContainsAny(port, "\r\n\x00") {
		return "", Exit(2, "authoritative SSH host key requires a valid host and port")
	}
	return knownhosts.Normalize(net.JoinHostPort(host, port)), nil
}

func writeAuthoritativeKnownHosts(path string, entry []byte) error {
	dir := filepath.Dir(path)
	if dir == path {
		return fmt.Errorf("isolated known_hosts path has no parent directory")
	}
	boundary, err := privateDirectoryDurabilityBoundary(dir, dir)
	if err != nil {
		return fmt.Errorf("resolve trusted known_hosts boundary: %w", err)
	}
	if err := inspectDirectoryPathWithoutSymlinks(dir, boundary); err != nil {
		return fmt.Errorf("inspect known_hosts parent path without symlinks: %w", err)
	}
	if err := secureAuthoritativeKnownHostsPath(dir, true); err != nil {
		return err
	}

	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular known_hosts target")
		}
		if info.Size() == int64(len(entry)) {
			current, readErr := os.ReadFile(path)
			if readErr != nil {
				return fmt.Errorf("read existing known_hosts target: %w", readErr)
			}
			if bytes.Equal(current, entry) {
				return secureAuthoritativeKnownHostsPath(path, false)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect known_hosts target: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".crabbox-known-hosts-*")
	if err != nil {
		return fmt.Errorf("create temporary known_hosts: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure temporary known_hosts: %w", err)
	}
	if _, err := tmp.Write(entry); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary known_hosts: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary known_hosts: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary known_hosts: %w", err)
	}
	if current, statErr := os.Lstat(path); statErr == nil {
		if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() {
			return fmt.Errorf("refusing replaced non-regular known_hosts target")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("reinspect known_hosts target: %w", statErr)
	}
	if err := replaceControllerFile(tmpPath, path); err != nil {
		return fmt.Errorf("atomically replace known_hosts: %w", err)
	}
	if err := secureAuthoritativeKnownHostsPath(path, false); err != nil {
		return err
	}
	if err := syncControllerDirectory(dir); err != nil {
		return fmt.Errorf("sync known_hosts directory: %w", err)
	}
	return nil
}

func secureAuthoritativeKnownHostsPath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect private SSH trust path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || directory && !info.IsDir() || !directory && !info.Mode().IsRegular() {
		return fmt.Errorf("private SSH trust path has an unsafe file type")
	}
	if err := secureSSHTransportPath(path, directory); err != nil {
		return fmt.Errorf("restrict private SSH trust permissions: %w", err)
	}
	return nil
}

func removeStoredTestboxConnectionArtifacts(ctx context.Context, leaseID string) error {
	key, err := TestboxKeyPath(leaseID)
	if err != nil {
		return err
	}
	dir, err := inspectTestboxLeaseDirectory(leaseID)
	if errors.Is(err, os.ErrNotExist) {
		return closeLeaseSSHControlMasters(ctx, filepath.Dir(key))
	}
	if err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return closeLeaseSSHControlMasters(ctx, dir)
	}
	if err != nil {
		return fmt.Errorf("inspect lease SSH directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("refusing to remove non-directory lease SSH path")
	}
	if err := closeLeaseSSHControlMasters(ctx, dir); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove lease SSH directory: %w", err)
	}
	return nil
}

// Build the hop only once the per-lease identity and trust paths have been selected.
// OpenSSH invokes ProxyCommand through a shell; quote every exact argument and
// escape its percent expansion before it reaches that shell.
func prepareSSHJump(target *SSHTarget) error {
	jump := target.SSHJump
	if net.ParseIP(jump.Host) == nil || jump.Port < 1 || jump.Port > 65535 || target.Key == "" {
		return Exit(2, "invalid SSH jump endpoint or missing lease identity")
	}
	key, err := parseAuthoritativeSSHHostKey(jump.HostKey)
	if err != nil {
		return err
	}
	if key.Type() != xssh.KeyAlgoED25519 || (jump.Fingerprint != "" && jump.Fingerprint != xssh.FingerprintSHA256(key)) {
		return Exit(2, "invalid SSH jump host key or fingerprint")
	}
	if err := selectSSHJumpCandidate(target, key); err != nil {
		return err
	}
	args := []string{"ssh", "-F", os.DevNull, "-i", target.Key, "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + sshConfigFileValue(target.KnownHostsFile), "-o", "GlobalKnownHostsFile=none", "-o", "KnownHostsCommand=none", "-o", "VerifyHostKeyDNS=no", "-o", "UpdateHostKeys=no", "-o", "HostKeyAlgorithms=ssh-ed25519", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15", "-p", strconv.Itoa(jump.Port), "-W", net.JoinHostPort(target.Host, target.Port), "crabbox@" + jump.Host}
	if jump.useRelay {
		proxy, err := prepareServeRelayProxy(target)
		if err != nil {
			return err
		}
		args = append(args[:1], append([]string{"-o", "ProxyCommand=" + proxy}, args[1:]...)...)
	}
	if jump.HostKeyAlias != "" {
		if _, err := authoritativeKnownHostToken(SSHTarget{HostKeyAlias: jump.HostKeyAlias}); err != nil {
			return err
		}
		args = append(args[:1], append([]string{"-o", "HostKeyAlias=" + jump.HostKeyAlias}, args[1:]...)...)
	}
	for i, arg := range args {
		args[i] = ShellQuote(strings.ReplaceAll(arg, "%", "%%"))
	}
	target.ProxyCommand = strings.Join(args, " ")
	target.NoControlMaster = true
	return nil
}

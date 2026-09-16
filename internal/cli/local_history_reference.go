package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/url"
	"strings"
)

// A local correlation key is not an attestation or a persisted credential URL.
func coordinatorHistoryReference(base string) string {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Hostname() == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port != "" && !(scheme == "https" && port == "443") && !(scheme == "http" && port == "80") {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	endpoint := scheme + "://" + host + strings.TrimRight(u.EscapedPath(), "/")
	digest := sha256.Sum256([]byte(endpoint))
	return "sha256:" + hex.EncodeToString(digest[:])
}

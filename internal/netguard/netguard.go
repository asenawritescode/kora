// Package netguard decides whether outbound requests may reach a host.
package netguard

import (
	"net"
	"strings"
)

// IsPrivateHost reports whether host names or is a private, loopback or
// link-local address.
func IsPrivateHost(host string) bool {
	privSuffixes := []string{
		".local", ".internal", ".localhost",
	}
	for _, s := range privSuffixes {
		if strings.HasSuffix(host, s) {
			return true
		}
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil {
		return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
	}
	return false
}

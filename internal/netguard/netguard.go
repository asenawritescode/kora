// Package netguard decides whether outbound requests may reach a host.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
	"time"
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

// ErrDisallowedDestination reports an outbound destination that resolves to a
// private, loopback, link-local, multicast or unspecified address.
var ErrDisallowedDestination = errors.New("destination is a private or internal address")

var carrierGradeNAT = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
var thisNetwork = &net.IPNet{IP: net.IPv4(0, 0, 0, 0), Mask: net.CIDRMask(8, 32)}

// IsDisallowedIP reports whether ip must never be reached by tenant-controlled
// outbound requests. Link-local covers cloud metadata (169.254.169.254).
func IsDisallowedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		carrierGradeNAT.Contains(ip) || thisNetwork.Contains(ip)
}

// Resolver looks up a host's addresses.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// CheckURL accepts an absolute http(s) URL only when its host and every
// address it resolves to are public.
func CheckURL(ctx context.Context, raw string, resolver Resolver) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return fmt.Errorf("URL must include a host")
	}
	if IsPrivateHost(host) {
		return fmt.Errorf("%w: %s", ErrDisallowedDestination, host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if IsDisallowedIP(ip) {
			return fmt.Errorf("%w: %s", ErrDisallowedDestination, host)
		}
		return nil
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("resolving %s: no addresses", host)
	}
	for _, addr := range addrs {
		if IsDisallowedIP(addr.IP) {
			return fmt.Errorf("%w: %s resolves to %s", ErrDisallowedDestination, host, addr.IP)
		}
	}
	return nil
}

// PublicDialer returns a dialer that refuses to connect to disallowed
// addresses. The check runs on the resolved address at connect time, so a
// hostname that re-resolves to an internal address is still refused.
func PublicDialer(timeout, keepAlive time.Duration) *net.Dialer {
	return &net.Dialer{
		Timeout:   timeout,
		KeepAlive: keepAlive,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("%w: %s", ErrDisallowedDestination, address)
			}
			if IsDisallowedIP(net.ParseIP(host)) {
				return fmt.Errorf("%w: %s", ErrDisallowedDestination, host)
			}
			return nil
		},
	}
}

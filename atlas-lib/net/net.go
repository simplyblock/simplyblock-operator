package net

import (
	"fmt"
	"net"
	"net/url"
)

// blockedIPNets contains IP ranges that must never be targeted by user-supplied URLs
// (SSRF protection: RFC-1918, loopback, link-local, cloud IMDS, IPv6 equivalents).
var blockedIPNets = func() []*net.IPNet {
	cidrs := []string{
		"127.0.0.0/8",
		"169.254.0.0/16",
		"0.0.0.0/8",
		"::1/128",
		"fe80::/10",
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, n, _ := net.ParseCIDR(cidr)
		nets = append(nets, n)
	}
	return nets
}()

func IsBlockedIP(ip net.IP) bool {
	for _, n := range blockedIPNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ValidateExternalURL rejects URLs that are unsafe to forward to the backend:
// - scheme must be https
// - host must not be a blocked IP literal (RFC-1918, loopback, link-local)
// - hostname must resolve and all resolved IPs must not be blocked
func ValidateExternalURL(rawURL string) error {
	if rawURL == "" {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("malformed URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be https, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL must contain a hostname")
	}
	if ip := net.ParseIP(host); ip != nil {
		if IsBlockedIP(ip) {
			return fmt.Errorf("URL targets a restricted IP address (%s)", host)
		}
		return nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("cannot resolve host %q: %w", host, err)
	}
	for _, addr := range addrs {
		if ip := net.ParseIP(addr); ip != nil && IsBlockedIP(ip) {
			return fmt.Errorf("URL resolves to a restricted IP address (%s)", addr)
		}
	}
	return nil
}

// Package edgeworker is the edge worker: it claims scrape jobs from the zapply
// API, reads each page on this machine and posts the result back. The jobs come
// from other users, so every address it opens is checked first: it never reaches
// into the home network it runs on.
package edgeworker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
)

// Resolver looks up a host's addresses; net.DefaultResolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Guard refuses addresses that are not on the public internet.
type Guard struct {
	Resolver Resolver
	// allowPrivate lets tests reach httptest servers on loopback.
	allowPrivate bool
}

// NewGuard returns a guard that resolves names with the system resolver.
func NewGuard() *Guard { return &Guard{Resolver: net.DefaultResolver} }

// ErrRefused is an address the worker will not open.
var ErrRefused = errors.New("address refused")

var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8",       // "this" network
		"100.64.0.0/10",   // carrier-grade NAT, which is also Tailscale's range
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"240.0.0.0/4",     // reserved, and broadcast
		"64:ff9b::/96",    // NAT64: embeds an IPv4 address
		"64:ff9b:1::/48",
		"100::/64",      // discard-only
		"2001::/32",     // Teredo
		"2001:db8::/32", // documentation
		"2002::/16",     // 6to4: embeds an IPv4 address
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// blockedIP reports whether a is loopback, private, link-local, multicast,
// unspecified or otherwise not a public unicast address.
func blockedIP(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

var internalSuffixes = []string{".localhost", ".local", ".internal", ".lan", ".home", ".home.arpa", ".localdomain"}

// CheckURL accepts only http(s) URLs on the default ports whose host resolves
// to public addresses alone. A name with even one private address is refused,
// since the browser may pick it.
func (g *Guard) CheckURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: not a URL", ErrRefused)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: only http and https are opened", ErrRefused)
	}
	if u.User != nil {
		return fmt.Errorf("%w: the URL carries credentials", ErrRefused)
	}
	if g.allowPrivate {
		return nil
	}
	if p := u.Port(); p != "" && p != "80" && p != "443" {
		return fmt.Errorf("%w: port %s", ErrRefused, p)
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return fmt.Errorf("%w: no host", ErrRefused)
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if blockedIP(a) {
			return fmt.Errorf("%w: %s is not a public address", ErrRefused, host)
		}
		return nil
	}
	if host == "localhost" {
		return fmt.Errorf("%w: localhost", ErrRefused)
	}
	for _, s := range internalSuffixes {
		if strings.HasSuffix(host, s) {
			return fmt.Errorf("%w: %s is an internal name", ErrRefused, host)
		}
	}
	addrs, err := g.Resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, a := range addrs {
		if blockedIP(a) {
			return fmt.Errorf("%w: %s resolves to a non-public address", ErrRefused, host)
		}
	}
	return nil
}

// DialControl is a net.Dialer Control function that refuses the connection
// itself when the address being dialled is not public. It checks what is really
// connected to, so a name that changes its answer between the check and the
// connection (DNS rebinding) is still refused. The headless browser's
// connections are dialled with it too, through the guarded proxy.
func (g *Guard) DialControl(network, address string, _ syscall.RawConn) error {
	if g.allowPrivate {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRefused, err)
	}
	a, err := netip.ParseAddr(host)
	if err != nil || blockedIP(a) {
		return fmt.Errorf("%w: connecting to %s", ErrRefused, host)
	}
	return nil
}

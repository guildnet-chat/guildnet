// SPDX-License-Identifier: AGPL-3.0-only

// Package egress is how GuildNet calls addresses its customers choose (an
// agent's webhook): only HTTPS, only public addresses, checked after
// resolving and dialed exactly as checked, so a name can't be pointed
// inside between the check and the call (DNS rebinding); no redirects, and
// short timeouts. Inside GuildNet's VM a firewall refuses the same ranges
// too; this is the first line.
package egress

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Options configure a client.
type Options struct {
	// Resolve looks a name up; nil means the system's resolver.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// RootCAs, when set, are the CAs trusted instead of the system's (tests).
	RootCAs *x509.CertPool
	// AllowLoopback lets this machine be reached (tests only).
	AllowLoopback bool
	// PrivateHosts are names (or addresses) reached wherever they are, on a
	// private network too: a self-hosted install's own git server, say.
	PrivateHosts []string
}

// private says whether host is one of o's private hosts.
func (o Options) private(host string) bool {
	host = strings.TrimSuffix(host, ".")
	for _, h := range o.PrivateHosts {
		if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(h), "."), host) {
			return true
		}
	}
	return false
}

// Not public: private, shared, reserved, documentation and benchmark ranges,
// besides what netip already knows (loopback, link-local, multicast).
var notPublic = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3",
		"::/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "fec0::/10", "2001::/23", "2001:db8::/32", "2002::/16", "fc00::/7", "fe80::/10", "ff00::/8"} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// public says whether ip is a public address (loopback too, if allowed).
func public(ip netip.Addr, allowLoopback bool) bool {
	ip = ip.Unmap()
	if allowLoopback && ip.IsLoopback() {
		return true
	}
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// errNotPublic is what calling inside comes to.
var errNotPublic = errors.New("not a public address")

// CheckURL says what's wrong with raw as an address to call, if anything:
// it must be https, with a host and no user or password, and not a private
// address or a name that's plainly inside (localhost, *.internal, *.local).
func CheckURL(raw string) error { return Options{}.Check(raw) }

// Check is CheckURL, but for o's private hosts, which may be inside.
func (o Options) Check(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return errors.New("it must be an https:// address, with no user name or password in it")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if o.private(host) {
		return nil
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !public(ip, false) {
			return fmt.Errorf("%s is %w", host, errNotPublic)
		}
		return nil
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") ||
		strings.HasSuffix(host, ".local") || !strings.Contains(host, ".") {
		return fmt.Errorf("%s is %w", host, errNotPublic)
	}
	return nil
}

// Dial connects only to public addresses: a name is looked up once, every
// address it has must be public (so the one dialed can't be swapped for an
// inside one), and that address is what's dialed.
func Dial(o Options) func(ctx context.Context, network, addr string) (net.Conn, error) {
	resolve := o.Resolve
	if resolve == nil {
		resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips := []netip.Addr{}
		if ip, err := netip.ParseAddr(host); err == nil {
			ips = append(ips, ip)
		} else if ips, err = resolve(ctx, host); err != nil {
			return nil, fmt.Errorf("looking up %s: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("%s has no address", host)
		}
		for _, ip := range ips {
			if !public(ip, o.AllowLoopback) && !o.private(host) {
				return nil, fmt.Errorf("%s resolves to %s, which is %w", host, ip, errNotPublic)
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].Unmap().String(), port))
	}
}

// Client is an HTTP client that only reaches public HTTPS addresses.
func Client(o Options) *http.Client {
	transport := &http.Transport{
		Proxy:                 nil, // never through a proxy the environment names
		DialContext:           Dial(o),
		TLSClientConfig:       &tls.Config{RootCAs: o.RootCAs, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 25 * time.Second,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects aren't followed")
		},
	}
}

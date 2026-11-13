// Package egress limits where the operator's backend connections may go (user
// decision 2, 2026-11-03). A namespaced ObjectStore lets a tenant aim the
// operator at any host, so every connection is checked after the host name has
// been resolved, at connect time, against a deny list chosen by an operator
// environment variable. It also dials through the server's proxy, which may
// not carry credentials (user decision 1).
package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// EnvMode names the environment variable that selects the Mode.
const EnvMode = "EGRESS_MODE"

// Mode selects which destination ranges are blocked.
type Mode string

// Values of Mode.
const (
	// ModeStrict, the default, blocks loopback, link-local and the cloud
	// metadata addresses, and the private ranges (RFC 1918, CGNAT, ULA).
	ModeStrict Mode = "strict"
	// ModeAllowPrivate blocks only loopback, link-local and the metadata
	// addresses, for clusters whose object stores are on private addresses.
	ModeAllowPrivate Mode = "allow-private"
)

var (
	// ErrBlocked means the destination is in a blocked range.
	ErrBlocked = errors.New("destination is blocked by the egress policy")
	// ErrProxyUserinfo means the proxy URL carries user:password@.
	ErrProxyUserinfo = errors.New("proxy URL must not carry credentials")
	// ErrInvalidMode means EGRESS_MODE is not a Mode.
	ErrInvalidMode = errors.New("invalid egress mode")
	// ErrProxy means the proxy refused or could not set up the tunnel.
	ErrProxy = errors.New("proxy tunnel failed")
)

func prefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(cidrs))
	for i, c := range cidrs {
		out[i] = netip.MustParsePrefix(c)
	}
	return out
}

var (
	// alwaysBlocked holds in both modes: unspecified, loopback, link-local
	// (which contains 169.254.169.254), multicast and the reserved top of the
	// IPv4 space, plus metadata addresses outside those ranges.
	alwaysBlocked = prefixes(
		"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "fe80::/10", "ff00::/8",
		"100.100.100.200/32", "fd00:ec2::254/128",
		// IETF protocol assignments (which hold 192.0.0.192, Oracle's
		// metadata address), benchmarking, and Azure's wire server.
		"192.0.0.0/24", "198.18.0.0/15", "168.63.129.16/32",
	)
	// privateBlocked is added in ModeStrict.
	privateBlocked = prefixes("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fc00::/7")
)

// Policy is the deny list of one Mode. The zero Policy blocks nothing; tests
// use it, the operator builds its Policy with NewPolicy.
type Policy struct {
	blocked []netip.Prefix
}

// ParseMode parses EGRESS_MODE: empty is ModeStrict.
func ParseMode(v string) (Mode, error) {
	switch Mode(v) {
	case "", ModeStrict:
		return ModeStrict, nil
	case ModeAllowPrivate:
		return ModeAllowPrivate, nil
	default:
		return "", fmt.Errorf("%w %q: allowed values are %q and %q", ErrInvalidMode, v, ModeStrict, ModeAllowPrivate)
	}
}

// NewPolicy returns the Policy of a Mode.
func NewPolicy(m Mode) Policy {
	blocked := append([]netip.Prefix(nil), alwaysBlocked...)
	if m != ModeAllowPrivate {
		blocked = append(blocked, privateBlocked...)
	}
	return Policy{blocked: blocked}
}

// Check returns ErrBlocked when ip, or the IPv4 address it carries, is in a
// blocked range. An IPv6 address can carry an IPv4 one that a gateway then
// reaches: IPv4-mapped (::ffff:), NAT64 (64:ff9b::/96 and 64:ff9b:1::/48),
// 6to4 (2002::/16) and Teredo (2001::/32, the client address, inverted).
func (p Policy) Check(ip netip.Addr) error {
	ip = ip.Unmap().WithZone("")
	if err := p.checkOne(ip); err != nil {
		return err
	}
	if v4, ok := embeddedIPv4(ip); ok {
		return p.checkOne(v4)
	}
	return nil
}

func (p Policy) checkOne(ip netip.Addr) error {
	for _, pre := range p.blocked {
		if pre.Contains(ip) {
			return fmt.Errorf("%w: %s", ErrBlocked, ip)
		}
	}
	return nil
}

var (
	nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local     = netip.MustParsePrefix("64:ff9b:1::/48")
	sixToFour      = netip.MustParsePrefix("2002::/16")
	teredo         = netip.MustParsePrefix("2001::/32")
)

// embeddedIPv4 returns the IPv4 address an IPv6 transition address carries.
func embeddedIPv4(ip netip.Addr) (netip.Addr, bool) {
	if !ip.Is6() {
		return netip.Addr{}, false
	}
	b := ip.As16()
	switch {
	case nat64WellKnown.Contains(ip):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case nat64Local.Contains(ip):
		// RFC 6052 section 2.2, /48: the address skips the u octet (b[8]).
		return netip.AddrFrom4([4]byte{b[6], b[7], b[9], b[10]}), true
	case sixToFour.Contains(ip):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	case teredo.Contains(ip):
		return netip.AddrFrom4([4]byte{^b[12], ^b[13], ^b[14], ^b[15]}), true
	}
	return netip.Addr{}, false
}

// control is net.Dialer.Control: it runs after name resolution, once per
// address the dialer is about to connect to.
func (p Policy) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrBlocked, address)
	}
	return p.Check(ap.Addr())
}

// DialFunc dials a TCP address.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Dialer returns a DialFunc that connects directly, or through proxy when it
// is not nil. Every address it connects to, the proxy's included, passes the
// Policy after resolution. With a proxy the destination must be an IP address
// (see checkTarget); HostDialer resolves a name first, for HTTP clients.
func (p Policy) Dialer(proxy *url.URL) (DialFunc, error) {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: p.control}
	if proxy == nil {
		return d.DialContext, nil
	}
	if proxy.User != nil {
		return nil, ErrProxyUserinfo
	}
	switch proxy.Scheme {
	case "socks5":
		// SOCKS5 fails only for a nil forward dialer, and a *net.Dialer is a
		// ContextDialer.
		sd, _ := xproxy.SOCKS5("tcp", proxyAddr(proxy), nil, d)
		cd, _ := sd.(xproxy.ContextDialer)
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			if err := p.checkTarget(addr); err != nil {
				return nil, err
			}
			return cd.DialContext(ctx, network, addr)
		}, nil
	case "http", "https":
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			if err := p.checkTarget(addr); err != nil {
				return nil, err
			}
			return connect(ctx, d, proxy, network, addr)
		}, nil
	default:
		return nil, fmt.Errorf("%w: scheme %q", ErrProxy, proxy.Scheme)
	}
}

// checkTarget checks the destination handed to a proxied dial. It must be an
// IP address: a host name would be resolved by the proxy, where this policy
// cannot see the answer, so it fails closed. HostDialer resolves names itself
// and hands the dialer addresses.
func (p Policy) checkTarget(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrBlocked, addr)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: %q is not an IP address", ErrBlocked, host)
	}
	return p.Check(ip)
}

// lookup resolves a host name; a variable so a test can answer.
var lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// HostDialer is Dialer for a client that hands the dialer a host name, as
// net/http does. Without a proxy it is Dialer: the dialer resolves the name
// and Control checks each address. With a proxy the name would be resolved by
// the proxy, where this Policy cannot see the answer, so it is resolved here,
// every address is checked (one blocked address refuses the name, so a record
// set that mixes a public and a blocked address cannot be used to slip
// through), and the proxy is asked for an address, never for the name.
func (p Policy) HostDialer(proxy *url.URL) (DialFunc, error) {
	dial, err := p.Dialer(proxy)
	if err != nil || proxy == nil {
		return dial, err
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("%w: %q", ErrBlocked, addr)
		}
		if _, err := netip.ParseAddr(host); err == nil {
			return dial(ctx, network, addr)
		}
		ips, err := lookup(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("%w: %q does not resolve", ErrBlocked, host)
		}
		for _, ip := range ips {
			if err := p.Check(ip); err != nil {
				return nil, err
			}
		}
		var last error
		for _, ip := range ips {
			var c net.Conn
			if c, last = dial(ctx, network, net.JoinHostPort(ip.String(), port)); last == nil {
				return c, nil
			}
		}
		return nil, last
	}, nil
}

// proxyAddr is the host:port of the proxy: the URL's port, or the scheme's
// default (http 80, https 443, socks5 1080) when it names none.
func proxyAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443", "socks5": "1080"}[u.Scheme]
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// proxyTLSConfig is the TLS configuration for an https proxy; tests replace
// it to trust their proxy's certificate.
var proxyTLSConfig = func() *tls.Config { return &tls.Config{MinVersion: tls.VersionTLS12} }

// connect opens an HTTP CONNECT tunnel to addr through the proxy.
func connect(ctx context.Context, d *net.Dialer, proxy *url.URL, network, addr string) (net.Conn, error) {
	conn, err := d.DialContext(ctx, network, proxyAddr(proxy))
	if err != nil {
		return nil, err
	}
	if proxy.Scheme == "https" {
		cfg := proxyTLSConfig()
		cfg.ServerName = proxy.Hostname()
		tc := tls.Client(conn, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%w: %w", ErrProxy, err)
		}
		conn = tc
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: http.Header{}}
	var resp *http.Response
	err = req.Write(conn)
	if err == nil {
		resp, err = http.ReadResponse(bufio.NewReader(conn), req)
	}
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: %w", ErrProxy, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: proxy answered %s", ErrProxy, resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

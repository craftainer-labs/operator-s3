package egress

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeStrict, "strict": ModeStrict, "allow-private": ModeAllowPrivate} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseMode("off"); !errors.Is(err, ErrInvalidMode) {
		t.Errorf("ParseMode(off) = %v", err)
	}
}

// The CIDR tables of both modes: the default blocks loopback, link-local and
// metadata and the private ranges; allow-private blocks only the first group.
func TestPolicyCheck(t *testing.T) {
	always := []string{
		"0.0.0.0", "127.0.0.1", "127.255.255.254", "169.254.169.254", "169.254.0.1", "224.0.0.1", "255.255.255.255",
		"::", "::1", "fe80::1", "ff02::1", "fd00:ec2::254", "100.100.100.200", "::ffff:127.0.0.1", "::ffff:169.254.169.254",
		"fe80::1%eth0",
		"192.0.0.192", "192.0.0.1", "198.18.0.1", "198.19.255.255", "168.63.129.16",
		// IPv4 carried by IPv6 transition addresses (a gateway reaches it):
		// NAT64 well-known and local-use prefixes, 6to4 and Teredo, all
		// carrying 169.254.169.254.
		"64:ff9b::a9fe:a9fe", "64:ff9b:1:a9fe:a9:fe00::", "2002:a9fe:a9fe::1", "2001:0:4136:e378:8000:63bf:5601:5601",
	}
	private := []string{"10.0.0.1", "10.255.255.255", "172.16.0.1", "172.31.255.255", "192.168.1.1", "100.64.0.1", "fc00::1", "fd12:3456::1", "::ffff:10.1.2.3",
		"64:ff9b::a01:203", "64:ff9b:1:a01:2:300::", "2002:a01:203::1", "2001:0:4136:e378:8000:63bf:f5fe:fdfc"}
	public := []string{"8.8.8.8", "1.1.1.1", "172.15.255.255", "172.32.0.1", "192.169.0.1", "100.63.255.255", "100.128.0.1", "2001:db8::1", "2606:4700::1111", "11.0.0.1",
		"64:ff9b::808:808", "2002:808:808::1", "2001:0:4136:e378:8000:63bf:f7f7:f7f7", "198.17.255.255", "198.20.0.0", "192.0.1.1"}

	strict, allow := NewPolicy(ModeStrict), NewPolicy(ModeAllowPrivate)
	for _, s := range always {
		ip := netip.MustParseAddr(s)
		for name, p := range map[string]Policy{"strict": strict, "allow-private": allow} {
			if err := p.Check(ip); !errors.Is(err, ErrBlocked) {
				t.Errorf("%s: %s is not blocked: %v", name, s, err)
			}
		}
	}
	for _, s := range private {
		ip := netip.MustParseAddr(s)
		if err := strict.Check(ip); !errors.Is(err, ErrBlocked) {
			t.Errorf("strict: %s is not blocked: %v", s, err)
		}
		if err := allow.Check(ip); err != nil {
			t.Errorf("allow-private: %s is blocked: %v", s, err)
		}
	}
	for _, s := range public {
		ip := netip.MustParseAddr(s)
		if err := strict.Check(ip); err != nil {
			t.Errorf("strict: %s is blocked: %v", s, err)
		}
		if err := allow.Check(ip); err != nil {
			t.Errorf("allow-private: %s is blocked: %v", s, err)
		}
	}
	if err := (Policy{}).Check(netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Errorf("the zero Policy blocks: %v", err)
	}
}

func TestControl(t *testing.T) {
	p := NewPolicy(ModeStrict)
	if err := p.control("tcp", "127.0.0.1:5432", nil); !errors.Is(err, ErrBlocked) {
		t.Errorf("loopback: %v", err)
	}
	if err := p.control("tcp", "8.8.8.8:5432", nil); err != nil {
		t.Errorf("public: %v", err)
	}
	if err := p.control("tcp", "not-an-address", nil); !errors.Is(err, ErrBlocked) {
		t.Errorf("unparsable: %v", err)
	}
}

// A real dial to a loopback listener is stopped by the Control hook after the
// name has been resolved ("localhost"), before any packet is sent.
func TestDirectDialIsCheckedAfterResolution(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	_, port, _ := net.SplitHostPort(l.Addr().String())

	blocked, err := NewPolicy(ModeStrict).Dialer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocked(context.Background(), "tcp", "localhost:"+port); !errors.Is(err, ErrBlocked) {
		t.Errorf("dial localhost: %v", err)
	}

	open, err := Policy{}.Dialer(nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := open(context.Background(), "tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial with the zero Policy: %v", err)
	}
	_ = c.Close()
}

func TestProxyUserinfoAndScheme(t *testing.T) {
	for _, raw := range []string{"http://user:pw@proxy:3128", "socks5://user@proxy:1080", "https://:pw@proxy"} {
		u, _ := url.Parse(raw)
		if _, err := (Policy{}).Dialer(u); !errors.Is(err, ErrProxyUserinfo) {
			t.Errorf("%s: %v", raw, err)
		}
	}
	u, _ := url.Parse("ftp://proxy:21")
	if _, err := (Policy{}).Dialer(u); !errors.Is(err, ErrProxy) {
		t.Errorf("ftp: %v", err)
	}
}

func TestSOCKS5ChecksTheDestination(t *testing.T) {
	u, _ := url.Parse("socks5://127.0.0.1:1080")
	dial, err := NewPolicy(ModeStrict).Dialer(u)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dial(context.Background(), "tcp", "10.9.9.9:5432"); !errors.Is(err, ErrBlocked) {
		t.Errorf("private destination: %v", err)
	}
}

func TestCheckTarget(t *testing.T) {
	p := NewPolicy(ModeStrict)
	if err := p.checkTarget("pg.example.com:5432"); !errors.Is(err, ErrBlocked) {
		t.Errorf("a host name must fail closed: %v", err)
	}
	if err := p.checkTarget("10.1.2.3:5432"); !errors.Is(err, ErrBlocked) {
		t.Errorf("private IP: %v", err)
	}
	if err := p.checkTarget("no-port"); !errors.Is(err, ErrBlocked) {
		t.Errorf("no port: %v", err)
	}
}

// proxyServer answers CONNECT with status and, on 200, echoes one line.
func proxyServer(t *testing.T, status int, useTLS bool) (*httptest.Server, *url.URL) {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "10.9.9.9:5432" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		if status != http.StatusOK {
			http.Error(w, "no", status)
			return
		}
		hj := w.(http.Hijacker)
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 200 Connection established\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(conn, conn)
	})
	var srv *httptest.Server
	if useTLS {
		srv = httptest.NewTLSServer(h)
	} else {
		srv = httptest.NewServer(h)
	}
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return srv, u
}

func echo(t *testing.T, c net.Conn) {
	t.Helper()
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
}

func TestHTTPProxyTunnel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, u := proxyServer(t, http.StatusOK, false)
	dial, err := Policy{}.Dialer(u)
	if err != nil {
		t.Fatal(err)
	}
	c, err := dial(ctx, "tcp", "10.9.9.9:5432")
	if err != nil {
		t.Fatal(err)
	}
	echo(t, c)

	// A refusal is an error, and the destination is checked before the proxy
	// is contacted.
	_, refuse := proxyServer(t, http.StatusForbidden, false)
	dial, _ = Policy{}.Dialer(refuse)
	if _, err := dial(ctx, "tcp", "10.9.9.9:5432"); !errors.Is(err, ErrProxy) {
		t.Errorf("403: %v", err)
	}
	dial, _ = NewPolicy(ModeStrict).Dialer(u)
	if _, err := dial(ctx, "tcp", "10.9.9.9:5432"); !errors.Is(err, ErrBlocked) {
		t.Errorf("a blocked destination reached the proxy path: %v", err)
	}
	// The proxy itself is checked after resolution too (it is on loopback).
	if _, err := dial(ctx, "tcp", "8.8.8.8:5432"); !errors.Is(err, ErrBlocked) {
		t.Errorf("a blocked proxy was dialled: %v", err)
	}
}

func TestHTTPSProxyTunnel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv, u := proxyServer(t, http.StatusOK, true)
	u.Host = "127.0.0.1:" + u.Port()

	old := proxyTLSConfig
	t.Cleanup(func() { proxyTLSConfig = old })
	proxyTLSConfig = func() *tls.Config {
		pool := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
		return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	}
	dial, err := Policy{}.Dialer(u)
	if err != nil {
		t.Fatal(err)
	}
	c, err := dial(ctx, "tcp", "10.9.9.9:5432")
	if err != nil {
		t.Fatal(err)
	}
	echo(t, c)

	// A proxy that does not speak TLS fails the handshake.
	plain, _ := proxyServer(t, http.StatusOK, false)
	pu, _ := url.Parse(plain.URL)
	pu.Scheme = "https"
	dial, _ = Policy{}.Dialer(pu)
	if _, err := dial(ctx, "tcp", "10.9.9.9:5432"); !errors.Is(err, ErrProxy) {
		t.Errorf("plain proxy over https: %v", err)
	}
}

// A proxy that is down is a dial error; one that hangs up is a proxy error.
func TestProxyFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	hangup, _ := url.Parse("http://" + addr)
	dial, _ := Policy{}.Dialer(hangup)
	if _, err := dial(ctx, "tcp", "10.9.9.9:5432"); !errors.Is(err, ErrProxy) {
		t.Errorf("hang-up: %v", err)
	}
	_ = l.Close()

	for _, scheme := range []string{"http", "socks5"} {
		down, _ := url.Parse(scheme + "://" + addr)
		dial, _ = Policy{}.Dialer(down)
		if _, err := dial(ctx, "tcp", "10.9.9.9:5432"); err == nil {
			t.Errorf("%s proxy that is down: no error", scheme)
		}
	}
}

func TestProxyAddrDefaultPorts(t *testing.T) {
	for raw, want := range map[string]string{
		"http://proxy.corp": "proxy.corp:80", "https://proxy.corp": "proxy.corp:443", "socks5://proxy.corp": "proxy.corp:1080",
		"http://proxy.corp:3128": "proxy.corp:3128", "socks5://[2001:db8::1]": "[2001:db8::1]:1080",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := proxyAddr(u); got != want {
			t.Errorf("proxyAddr(%s) = %s, want %s", raw, got, want)
		}
	}
}

// stubLookup replaces the resolver for one test.
func stubLookup(t *testing.T, answer func(host string) ([]netip.Addr, error)) {
	t.Helper()
	old := lookup
	t.Cleanup(func() { lookup = old })
	lookup = func(_ context.Context, host string) ([]netip.Addr, error) { return answer(host) }
}

func TestHostDialerWithoutAProxyIsTheDialer(t *testing.T) {
	dial, err := NewPolicy(ModeStrict).HostDialer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dial(context.Background(), "tcp", "localhost:1"); !errors.Is(err, ErrBlocked) {
		t.Errorf("loopback: %v", err)
	}
	u, _ := url.Parse("http://user:pw@proxy:3128")
	if _, err := (Policy{}).HostDialer(u); !errors.Is(err, ErrProxyUserinfo) {
		t.Errorf("userinfo: %v", err)
	}
}

// With a proxy a host name is resolved here, every address is checked, and the
// proxy is asked for an address, never for the name.
func TestHostDialerResolvesBeforeTheProxy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, u := proxyServer(t, http.StatusOK, false)
	addrs := map[string][]netip.Addr{
		"store.example.test":  {netip.MustParseAddr("10.9.9.9")},
		"second.example.test": {netip.MustParseAddr("10.9.9.8"), netip.MustParseAddr("10.9.9.9")},
		"mixed.example.test":  {netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("169.254.169.254")},
		"meta.example.test":   {netip.MustParseAddr("::ffff:169.254.169.254")},
		"none.example.test":   {},
		"down.example.test":   {netip.MustParseAddr("10.9.9.8")},
	}
	boom := errors.New("no such host")
	stubLookup(t, func(host string) ([]netip.Addr, error) {
		if a, ok := addrs[host]; ok {
			return a, nil
		}
		return nil, boom
	})

	dial, err := Policy{}.HostDialer(u)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"store.example.test", "second.example.test"} {
		c, err := dial(ctx, "tcp", host+":5432")
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		echo(t, c)
	}
	// An IP literal goes straight to the proxy path.
	c, err := dial(ctx, "tcp", "10.9.9.9:5432")
	if err != nil {
		t.Fatal(err)
	}
	echo(t, c)
	// The proxy answered 400 for the only address: its error is returned.
	if _, err := dial(ctx, "tcp", "down.example.test:5432"); !errors.Is(err, ErrProxy) {
		t.Errorf("only address refused: %v", err)
	}
	if _, err := dial(ctx, "tcp", "absent.example.test:5432"); !errors.Is(err, boom) {
		t.Errorf("resolver error: %v", err)
	}
	if _, err := dial(ctx, "tcp", "none.example.test:5432"); !errors.Is(err, ErrBlocked) {
		t.Errorf("no address: %v", err)
	}
	if _, err := dial(ctx, "tcp", "no-port"); !errors.Is(err, ErrBlocked) {
		t.Errorf("no port: %v", err)
	}

	strict, err := NewPolicy(ModeStrict).HostDialer(u)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"store.example.test", "mixed.example.test", "meta.example.test"} {
		if _, err := strict(ctx, "tcp", host+":5432"); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s under strict: %v", host, err)
		}
	}
}

func TestDefaultLookupResolves(t *testing.T) {
	ips, err := lookup(context.Background(), "localhost")
	if err != nil || len(ips) == 0 {
		t.Errorf("lookup(localhost) = %v, %v", ips, err)
	}
}

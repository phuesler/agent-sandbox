package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDomainList(t *testing.T) {
	d := parseDomainList("# c\n.github.com\nregistry.npmjs.org # x\n.Quay.IO\n\n")
	for host, want := range map[string]bool{
		"github.com": true, "api.github.com": true, "API.GitHub.com.": true,
		"evilgithub.com": false, "github.com.evil.net": false,
		"registry.npmjs.org": true, "x.registry.npmjs.org": false, "npmjs.org": false,
		"quay.io": true, "cdn.quay.io": true, "": false, "140.82.112.3": false,
	} {
		if got := d.allows(host); got != want {
			t.Errorf("allows(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestPublicIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"140.82.112.3": true, "1.1.1.1": true, "2606:4700::1111": true,
		"10.88.0.1": false, "192.168.1.1": false, "172.17.0.1": false, "127.0.0.1": false,
		"169.254.169.254": false, "100.100.1.1": false, "::1": false, "fd00::1": false, "fe80::1": false, "0.0.0.0": false,
	} {
		if got := publicIP(net.ParseIP(ip)); got != want {
			t.Errorf("publicIP(%s) = %v, want %v", ip, got, want)
		}
	}
}

// testProxy: allowlist "allowed.test", every name resolves to 127.0.0.1 and all
// dials go to target (whatever port was asked for).
func testProxy(t *testing.T, target string) (*egressProxy, string) {
	t.Helper()
	dir := t.TempDir()
	allow, open, logp := filepath.Join(dir, "allow"), filepath.Join(dir, "open"), filepath.Join(dir, "log")
	os.WriteFile(allow, []byte(".allowed.test\n"), 0o644)
	os.WriteFile(open, []byte("# none\n"), 0o644)
	p := newEgressProxy(allow, open, logp)
	p.resolve = func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	p.allowIP = func(net.IP) bool { return true }
	p.dialIP = func(ctx context.Context, ip net.IP, port string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", target)
	}
	if err := p.reload(); err != nil {
		t.Fatal(err)
	}
	return p, logp
}

func readLog(t *testing.T, path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func TestDialRefusesPrivateAddresses(t *testing.T) {
	p, _ := testProxy(t, "127.0.0.1:1")
	p.allowIP = publicIP // real policy
	p.resolve = func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("192.168.1.1")}, nil // allowlisted name pointing into the LAN
	}
	if _, err := p.dialContext(context.Background(), "tcp", "rebind.allowed.test:443"); err == nil ||
		!strings.Contains(err.Error(), "private") {
		t.Fatalf("expected refusal of private address, got %v", err)
	}
}

func TestExplicitHTTP(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "host=%s path=%s proxyconn=%q", r.Host, r.URL.Path, r.Header.Get("Proxy-Connection"))
	}))
	defer up.Close()
	p, logp := testProxy(t, strings.TrimPrefix(up.URL, "http://"))
	srv := httptest.NewServer(p)
	defer srv.Close()
	pu, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}

	resp, err := client.Get("http://www.allowed.test/pkg")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "host=www.allowed.test path=/pkg") || strings.Contains(string(body), `proxyconn="keep`) {
		t.Fatalf("allowed: %d %s", resp.StatusCode, body)
	}
	resp, err = client.Get("http://evil.test/x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 403 || strings.Contains(string(body), "squid") {
		t.Fatalf("denied: %d %s", resp.StatusCode, body)
	}
	time.Sleep(50 * time.Millisecond)
	d := parseDenials(strings.Split(readLog(t, logp), "\n"), map[string]string{"127.0.0.1": "sbx"})
	if len(d) != 1 || d[0].Host != "evil.test" || d[0].Who != "sbx" {
		t.Fatalf("log/net log parsing: %+v\n%s", d, readLog(t, logp))
	}
}

func TestExplicitConnectAndPorts(t *testing.T) {
	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	p, _ := testProxy(t, echo.Addr().String())
	srv := httptest.NewServer(p)
	defer srv.Close()
	connect := func(target string) (int, net.Conn) {
		c, _ := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, c
	}
	code, c := connect("api.allowed.test:443")
	if code != 200 {
		t.Fatalf("CONNECT allowed: %d", code)
	}
	c.Write([]byte("ping"))
	buf := make([]byte, 4)
	io.ReadFull(c, buf)
	c.Close()
	if string(buf) != "ping" {
		t.Fatalf("tunnel echo: %q", buf)
	}
	if code, c := connect("evil.test:443"); code != 403 {
		t.Errorf("CONNECT denied host: %d", code)
	} else {
		c.Close()
	}
	if code, c := connect("api.allowed.test:22"); code != 403 {
		t.Errorf("CONNECT to non-443 port must be denied: %d", code)
	} else {
		c.Close()
	}
	// an "open" sandbox may go anywhere (still only public addresses in production)
	os.WriteFile(p.openPath, []byte("127.0.0.1\n"), 0o644)
	p.reload()
	if code, c := connect("evil.test:22"); code != 200 {
		t.Errorf("open client: %d", code)
	} else {
		c.Close()
	}
}

func TestTransparentTLS(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "tls ok host=%s", r.Host)
	}))
	defer up.Close()
	p, logp := testProxy(t, strings.TrimPrefix(up.URL, "https://"))
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go p.handleTLS(c)
		}
	}()
	get := func(name string) (string, error) {
		tr := &http.Transport{
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, err := net.Dial("tcp", l.Addr().String()) // as if nftables redirected :443
				if err != nil {
					return nil, err
				}
				tc := tls.Client(c, &tls.Config{ServerName: name, InsecureSkipVerify: true})
				return tc, tc.HandshakeContext(ctx)
			},
		}
		defer tr.CloseIdleConnections() // the tunnel is logged when it closes
		resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get("https://" + name + "/")
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), nil
	}
	if body, err := get("www.allowed.test"); err != nil || body != "tls ok host=www.allowed.test" {
		t.Fatalf("allowed via SNI: %q %v", body, err)
	}
	if _, err := get("evil.test"); err == nil {
		t.Fatal("denied SNI must not connect")
	}
	time.Sleep(100 * time.Millisecond)
	log := readLog(t, logp)
	if !strings.Contains(log, "TCP_TUNNEL/200") || !strings.Contains(log, "TCP_DENIED/403") || !strings.Contains(log, "evil.test:443") {
		t.Fatalf("log:\n%s", log)
	}
}

func TestPeekSNINoName(t *testing.T) {
	a, b := net.Pipe()
	go func() {
		tls.Client(a, &tls.Config{InsecureSkipVerify: true}).Handshake() // no ServerName
		a.Close()
	}()
	b.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := peekSNI(b); err == nil {
		t.Fatal("expected error for ClientHello without SNI")
	}
}

func TestHostFilesEgress(t *testing.T) {
	t.Setenv("ABOX_CONFIG", filepath.Join(t.TempDir(), "none"))
	c, _ := loadConfig()
	files, _, err := hostFiles(c, "someuser")
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]string{}
	for _, f := range files {
		byPath[f.Path] = f.Content
	}
	if _, ok := byPath["/etc/abox/squid.conf"]; ok {
		t.Error("squid config must no longer be installed")
	}
	nft := byPath["/etc/abox/egress.nft"]
	for _, want := range []string{
		`tcp dport 443 redirect to :3129`, `tcp dport 80 redirect to :3130`,
		`ip saddr @open4 return`, `tcp dport { 53, 3128, 3129, 3130 } accept`,
		`iifname != { "aboxbr0", "lo" } tcp dport { 3128, 3129, 3130 } counter drop`,
	} {
		if !strings.Contains(nft, want) {
			t.Errorf("egress.nft lacks %q", want)
		}
	}
	if strings.Contains(nft, "@ABOX_") {
		t.Error("unrendered placeholder in egress.nft")
	}
	if a := byPath["/etc/abox/allowlist.txt"]; !parseDomainList(a).allows("api.anthropic.com") || parseDomainList(a).allows("example.com") {
		t.Error("allowlist.txt content")
	}
}

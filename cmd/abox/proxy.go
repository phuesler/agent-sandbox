package main

// Egress filter for sandboxes (replaces squid). Runs on the host as
// abox-proxy.service (`abox _proxy`) and enforces the domain allowlist:
//
//   - explicit proxy (http_proxy / https_proxy in the guests): CONNECT for
//     HTTPS, absolute-form requests for plain HTTP
//   - transparent HTTPS: nftables redirects the bridge's outgoing :443 here;
//     the hostname is read from the TLS ClientHello (SNI), nothing is decrypted
//   - transparent HTTP: nftables redirects :80 here; hostname from Host header
//
// Upstream connections are made by *name* (resolved on the host), never to the
// IP the guest picked, and never to private/loopback/link-local/CGNAT
// addresses. Everything listens on the bridge address only.

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------------------------------------------------------------- allowlist

// domainList implements squid dstdomain semantics:
// ".example.com" matches example.com and all subdomains, "example.com" only itself.
type domainList struct {
	exact    map[string]bool
	wildcard []string // ".example.com"
}

func parseDomainList(text string) *domainList {
	d := &domainList{exact: map[string]bool{}}
	for _, raw := range strings.Split(text, "\n") {
		line, _, _ := strings.Cut(raw, "#")
		e := strings.ToLower(strings.TrimSpace(line))
		switch {
		case e == "":
		case strings.HasPrefix(e, "."):
			d.wildcard = append(d.wildcard, e)
		default:
			d.exact[e] = true
		}
	}
	return d
}

func (d *domainList) allows(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || net.ParseIP(host) != nil { // IP literals are never allowlisted
		return false
	}
	if d.exact[host] {
		return true
	}
	for _, w := range d.wildcard {
		if host == w[1:] || strings.HasSuffix(host, w) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- address policy

var blockedNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// publicIP reports whether a sandbox may be connected to ip.
func publicIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- proxy

type egressProxy struct {
	allowPath, openPath, logPath string

	mu    sync.RWMutex
	allow *domainList
	open  map[string]bool

	logMu sync.Mutex
	logw  io.WriteCloser

	resolve   func(ctx context.Context, host string) ([]net.IP, error)
	allowIP   func(net.IP) bool                                                   // default publicIP
	dialIP    func(ctx context.Context, ip net.IP, port string) (net.Conn, error) // tests override
	dialer    net.Dialer
	transport *http.Transport
}

func newEgressProxy(allowPath, openPath, logPath string) *egressProxy {
	p := &egressProxy{allowPath: allowPath, openPath: openPath, logPath: logPath,
		dialer: net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}}
	p.resolve = func(ctx context.Context, host string) ([]net.IP, error) {
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		ips := make([]net.IP, 0, len(addrs))
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
		return ips, err
	}
	p.allowIP = publicIP
	p.dialIP = func(ctx context.Context, ip net.IP, port string) (net.Conn, error) {
		return p.dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
	}
	p.transport = &http.Transport{
		Proxy:                 nil, // never chain to another proxy
		DialContext:           p.dialContext,
		DisableCompression:    true, // pass the client's encoding through unchanged
		MaxIdleConns:          64,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return p
}

// reload re-reads the allowlist and the "open" clients (SIGHUP, abox-net).
func (p *egressProxy) reload() error {
	data, err := os.ReadFile(p.allowPath)
	if err != nil {
		return err
	}
	open := map[string]bool{}
	if od, err := os.ReadFile(p.openPath); err == nil {
		for _, l := range strings.Split(string(od), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				open[l] = true
			}
		}
	}
	p.mu.Lock()
	p.allow, p.open = parseDomainList(string(data)), open
	p.mu.Unlock()
	if p.logPath != "" {
		f, err := os.OpenFile(p.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		p.logMu.Lock()
		if p.logw != nil {
			p.logw.Close()
		}
		p.logw = f
		p.logMu.Unlock()
	}
	return nil
}

func (p *egressProxy) isOpen(client string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.open[client]
}

func (p *egressProxy) permitted(client, host string) bool {
	if p.isOpen(client) {
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.allow != nil && p.allow.allows(host)
}

// logf writes squid-style lines ("time elapsed client CODE/status bytes method url"),
// which `abox net log` parses.
func (p *egressProxy) logf(start time.Time, client, code string, status int, bytes int64, method, url string) {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	if p.logw == nil {
		return
	}
	now := time.Now()
	fmt.Fprintf(p.logw, "%d.%03d %6d %s %s/%d %d %s %s - abox\n", now.Unix(), now.Nanosecond()/1e6,
		now.Sub(start).Milliseconds(), client, code, status, bytes, method, url)
}

// dialContext resolves on the host and connects only to public addresses.
func (p *egressProxy) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else if ips, err = p.resolve(ctx, host); err != nil {
		return nil, err
	}
	var lastErr error = fmt.Errorf("%s resolves only to private or reserved addresses", host)
	for _, ip := range ips {
		if !p.allowIP(ip) {
			continue
		}
		c, err := p.dialIP(ctx, ip, port)
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func clientIP(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return h
}

func splitHostPortDefault(hostport, def string) (string, string) {
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return h, p
	}
	return strings.Trim(hostport, "[]"), def
}

// pipe copies in both directions until one side finishes; returns bytes sent to the client.
func pipe(client, upstream net.Conn) int64 {
	var down int64
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(upstream, client)
		if c, ok := upstream.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		down, _ = io.Copy(client, upstream)
		if c, ok := client.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
	client.Close()
	upstream.Close()
	return down
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func stripHop(h http.Header) {
	for _, f := range strings.Split(h.Get("Connection"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			h.Del(f)
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

// ServeHTTP handles the explicit proxy (CONNECT, absolute-form) and transparent
// plain HTTP (origin-form with Host header).
func (p *egressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start, client := time.Now(), clientIP(r.RemoteAddr)
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r, start, client)
		return
	}
	hostport := r.URL.Host
	if hostport == "" {
		hostport = r.Host
	}
	host, port := splitHostPortDefault(hostport, "80")
	url := "http://" + net.JoinHostPort(host, port) + r.URL.RequestURI()
	if (port != "80" && !p.isOpen(client)) || !p.permitted(client, host) {
		p.logf(start, client, "TCP_DENIED", 403, 0, r.Method, url)
		http.Error(w, "blocked by the abox egress allowlist: "+host, http.StatusForbidden)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme, out.URL.Host = "http", net.JoinHostPort(host, port)
	out.Host = r.Host
	if out.Host == "" {
		out.Host = host
	}
	stripHop(out.Header)
	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		p.logf(start, client, "TCP_MISS", 502, 0, r.Method, url)
		http.Error(w, "abox egress: upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	stripHop(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)
	p.logf(start, client, "TCP_MISS", resp.StatusCode, n, r.Method, url)
}

func (p *egressProxy) handleConnect(w http.ResponseWriter, r *http.Request, start time.Time, client string) {
	host, port := splitHostPortDefault(r.Host, "443")
	target := net.JoinHostPort(host, port)
	if (port != "443" && !p.isOpen(client)) || !p.permitted(client, host) {
		p.logf(start, client, "TCP_DENIED", 403, 0, "CONNECT", target)
		http.Error(w, "blocked by the abox egress allowlist: "+host, http.StatusForbidden)
		return
	}
	up, err := p.dialContext(r.Context(), "tcp", target)
	if err != nil {
		p.logf(start, client, "TCP_TUNNEL", 502, 0, "CONNECT", target)
		http.Error(w, "abox egress: "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	if n := buf.Reader.Buffered(); n > 0 { // client may have sent its ClientHello already
		b, _ := buf.Reader.Peek(n)
		up.Write(b)
	}
	n := pipe(conn, up)
	p.logf(start, client, "TCP_TUNNEL", 200, n, "CONNECT", target)
}

// ---------------------------------------------------------------- transparent TLS

var errSNIDone = errors.New("sni captured")

// recordingConn feeds the TLS server reads from the client and refuses writes,
// so a handshake can be started just far enough to parse the ClientHello.
type recordingConn struct {
	net.Conn
	r io.Reader
}

func (c recordingConn) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c recordingConn) Write(b []byte) (int, error) { return 0, io.ErrClosedPipe }

// peekSNI reads the ClientHello and returns the server name plus the bytes read,
// which must be replayed to the upstream server.
func peekSNI(conn net.Conn) (string, []byte, error) {
	var seen bytes.Buffer
	var name string
	cfg := &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		name = h.ServerName
		return nil, errSNIDone
	}}
	_ = tls.Server(recordingConn{Conn: conn, r: io.TeeReader(conn, &seen)}, cfg).Handshake()
	if name == "" {
		return "", seen.Bytes(), errors.New("no SNI in TLS ClientHello")
	}
	return name, seen.Bytes(), nil
}

func (p *egressProxy) handleTLS(conn net.Conn) {
	start, client := time.Now(), clientIP(conn.RemoteAddr().String())
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	name, hello, err := peekSNI(conn)
	conn.SetReadDeadline(time.Time{})
	if err != nil || !p.permitted(client, name) {
		if name == "" {
			name = "(no SNI)"
		}
		p.logf(start, client, "TCP_DENIED", 403, 0, "CONNECT", name+":443")
		conn.Close()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	up, err := p.dialContext(ctx, "tcp", net.JoinHostPort(name, "443"))
	cancel()
	if err != nil {
		p.logf(start, client, "TCP_TUNNEL", 502, 0, "CONNECT", name+":443")
		conn.Close()
		return
	}
	if _, err := up.Write(hello); err != nil {
		conn.Close()
		up.Close()
		return
	}
	n := pipe(conn, up)
	p.logf(start, client, "TCP_TUNNEL", 200, n, "CONNECT", name+":443")
}

// ---------------------------------------------------------------- serving

// listenFreebind binds even if the address does not exist yet (the bridge is
// created by incus, possibly after this service starts).
func listenFreebind(addr string) (net.Listener, error) {
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_IP, syscall.IP_FREEBIND, 1)
		})
		if err != nil {
			return err
		}
		return serr
	}}
	return lc.Listen(context.Background(), "tcp4", addr)
}

func proxyPorts(c *Config) (explicit, tlsPort, httpPort int) {
	explicit, _ = strconv.Atoi(c.ProxyPort)
	return explicit, explicit + 1, explicit + 2
}

// cmdProxy runs the egress filter (abox-proxy.service). Paths can be overridden
// for tests: ABOX_PROXY_LISTEN, ABOX_PROXY_ALLOWLIST, ABOX_PROXY_OPEN, ABOX_PROXY_LOG.
func cmdProxy(a *App) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	listen := env("ABOX_PROXY_LISTEN", cfg.HostIP())
	p := newEgressProxy(env("ABOX_PROXY_ALLOWLIST", "/etc/abox/allowlist.txt"),
		env("ABOX_PROXY_OPEN", "/etc/abox/open-clients"), env("ABOX_PROXY_LOG", "/var/log/abox-proxy/access.log"))
	if err := p.reload(); err != nil {
		return err
	}
	explicit, tlsPort, httpPort := proxyPorts(cfg)
	errs := make(chan error, 3)
	serveHTTP := func(port int) {
		l, err := listenFreebind(net.JoinHostPort(listen, strconv.Itoa(port)))
		if err != nil {
			errs <- err
			return
		}
		srv := &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second,
			ErrorLog: nil}
		errs <- srv.Serve(l)
	}
	go serveHTTP(explicit)
	go serveHTTP(httpPort)
	go func() {
		l, err := listenFreebind(net.JoinHostPort(listen, strconv.Itoa(tlsPort)))
		if err != nil {
			errs <- err
			return
		}
		for {
			c, err := l.Accept()
			if err != nil {
				errs <- err
				return
			}
			go p.handleTLS(c)
		}
	}()
	fmt.Fprintf(os.Stderr, "abox egress filter %s: explicit %s:%d, transparent https :%d, http :%d\n",
		version, listen, explicit, tlsPort, httpPort)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGTERM, os.Interrupt)
	for {
		select {
		case err := <-errs:
			return err
		case s := <-sig:
			if s != syscall.SIGHUP {
				return nil
			}
			if err := p.reload(); err != nil {
				fmt.Fprintln(os.Stderr, "reload failed:", err)
			} else {
				fmt.Fprintln(os.Stderr, "reloaded allowlist and open clients")
			}
		}
	}
}

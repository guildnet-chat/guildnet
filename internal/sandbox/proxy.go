// SPDX-License-Identifier: AGPL-3.0-only

package sandbox

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAllowed is where desks can reach unless a network says otherwise:
// GitHub, and the Go and PyPI registries (PyPI's uploads are elsewhere).
// npm's registry, which takes uploads, is read through DefaultReads.
var DefaultAllowed = []string{
	"github.com", "api.github.com", "codeload.github.com", "*.githubusercontent.com",
	"proxy.golang.org", "sum.golang.org", "dl.google.com",
	"pypi.org", "files.pythonhosted.org",
}

// DefaultReads are the registries desks read through the proxy, and can't
// publish to (Proxy.Reads).
var DefaultReads = map[string]string{"/npm/": "https://registry.npmjs.org/"}

// Proxy is the desks' only way out: an HTTPS tunnel (CONNECT) to an allowed
// host, by name, on an allowed port, dialed only at public addresses.
type Proxy struct {
	// Allowed are host names, or *.domain for any name under it.
	Allowed []string
	// Ports are the ports tunnels may go to; none means 443.
	Ports []int
	// Keys check networks' egress passes, which open their git hosts too
	// (sent as the proxy password); none means no pass is taken. Now is the
	// time to check them at; nil means time.Now.
	Keys []ed25519.PublicKey
	Now  func() time.Time
	// Dial connects to a host (egress.Dial: public addresses only).
	Dial   func(ctx context.Context, network, addr string) (net.Conn, error)
	Logger *log.Logger
	// Idle is how long a tunnel nothing goes through stays open; none means
	// 5 minutes. MaxTunnels is how many one desk (one address) may have open
	// at once; none means 64. The proxy is every desk's, so one mustn't use
	// it all up.
	Idle       time.Duration
	MaxTunnels int
	// Reads are ways to read registries that take uploads, by path prefix
	// ("/npm/"): GET and HEAD go to the upstream base URL, without the desk's
	// credentials, and nothing else does. A desk reads packages there, and
	// can't publish one, whatever token it was given. ReadClient fetches
	// them; nil means one dialing with Dial.
	Reads      map[string]string
	ReadClient *http.Client

	mu    sync.Mutex
	open  map[string]int // desk's address -> tunnels open
	reads *http.Client
}

// take counts a tunnel from addr, if it's within its share.
func (p *Proxy) take(addr string) bool {
	most := p.MaxTunnels
	if most <= 0 {
		most = 64
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.open == nil {
		p.open = map[string]int{}
	}
	if p.open[addr] >= most {
		return false
	}
	p.open[addr]++
	return true
}

func (p *Proxy) give(addr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.open[addr]--; p.open[addr] <= 0 {
		delete(p.open, addr)
	}
}

func (p *Proxy) logf(format string, args ...any) {
	if p.Logger != nil {
		p.Logger.Printf(format, args...)
	}
}

// allows says whether host is on the list.
func (p *Proxy) allows(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, a := range p.Allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if suffix, ok := strings.CutPrefix(a, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
		} else if host == a {
			return true
		}
	}
	return false
}

// passOpens says whether r carries an egress pass opening host on port.
func (p *Proxy) passOpens(r *http.Request, host string, port int) bool {
	auth, ok := strings.CutPrefix(r.Header.Get("Proxy-Authorization"), "Basic ")
	if !ok || len(p.Keys) == 0 {
		return false
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(auth))
	if err != nil {
		return false
	}
	_, pass, _ := strings.Cut(string(b), ":")
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	return PassOpens(p.Keys, pass, now(), host, port)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		for prefix, upstream := range p.Reads {
			if rest, ok := strings.CutPrefix(r.URL.Path, prefix); ok {
				p.read(w, r, upstream, rest)
				return
			}
		}
		http.Error(w, "only HTTPS tunnels (CONNECT) go through here", http.StatusMethodNotAllowed)
		return
	}
	host, portText, err := net.SplitHostPort(r.Host)
	port, _ := strconv.Atoi(portText)
	ports := p.Ports
	if len(ports) == 0 {
		ports = []int{443}
	}
	listed := err == nil && p.allows(host) && slices.Contains(ports, port)
	if !listed && (err != nil || !p.passOpens(r, host, port)) {
		p.logf("%s: refused %s", r.RemoteAddr, r.Host)
		http.Error(w, r.Host+" isn't on this desk's list of allowed hosts", http.StatusForbidden)
		return
	}
	desk, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !p.take(desk) {
		p.logf("%s: too many tunnels open, refused %s", r.RemoteAddr, r.Host)
		http.Error(w, "this desk has too many tunnels open: close some, or wait", http.StatusTooManyRequests)
		return
	}
	defer p.give(desk)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	upstream, err := p.Dial(ctx, "tcp", r.Host)
	cancel()
	if err != nil {
		p.logf("%s: %s: %v", r.RemoteAddr, r.Host, err)
		http.Error(w, "couldn't reach "+r.Host, http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "can't tunnel here", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n")
	p.logf("%s: tunnel to %s", r.RemoteAddr, r.Host)
	idle := p.Idle
	if idle <= 0 {
		idle = 5 * time.Minute
	}
	// Each way, the tunnel stays open while something goes through it.
	t := &tunnel{idle: idle, a: client, b: upstream}
	t.touch()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// What the client sent after its request, already read, goes first.
		if n := buf.Reader.Buffered(); n > 0 {
			b, _ := buf.Reader.Peek(n)
			upstream.Write(b)
		}
		t.copy(upstream, client)
		if c, ok := upstream.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		t.copy(client, upstream)
		if c, ok := client.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		}
	}()
	wg.Wait()
}

// readHeaders are the request headers a read passes on: what to send back,
// and caching. Never the desk's credentials.
var readHeaders = []string{"Accept", "Accept-Encoding", "If-None-Match", "If-Modified-Since", "Npm-Command", "User-Agent"}

// answerHeaders are the response headers a read passes back.
var answerHeaders = []string{"Content-Type", "Content-Encoding", "Content-Length", "ETag", "Last-Modified", "Cache-Control"}

// read gets path from upstream for a desk, if it only reads.
func (p *Proxy) read(w http.ResponseWriter, r *http.Request, upstream, path string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		p.logf("%s: refused %s %s (read only)", r.RemoteAddr, r.Method, r.URL.Path)
		http.Error(w, "this registry is read-only from desks: nothing can be published from one", http.StatusMethodNotAllowed)
		return
	}
	u := upstream + path
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u, nil)
	if err != nil {
		http.Error(w, "that isn't a path to read", http.StatusBadRequest)
		return
	}
	for _, h := range readHeaders {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	c := p.ReadClient
	if c == nil {
		c = p.readClient()
	}
	resp, err := c.Do(req)
	if err != nil {
		p.logf("%s: reading %s: %v", r.RemoteAddr, u, err)
		http.Error(w, "couldn't reach the registry", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range answerHeaders {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// readClient is a client for Reads, dialing with Dial (public addresses
// only), made once.
func (p *Proxy) readClient() *http.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reads == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy, t.DialContext = nil, p.Dial
		t.DisableCompression = true // the desk's own Accept-Encoding goes through
		t.ResponseHeaderTimeout = 30 * time.Second
		p.reads = &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return p.reads
}

// tunnel is a tunnel's two ends, closed once nothing has gone either way
// for idle.
type tunnel struct {
	idle time.Duration
	a, b net.Conn
}

// touch puts both ends' deadlines idle from now.
func (t *tunnel) touch() {
	at := time.Now().Add(t.idle)
	t.a.SetDeadline(at)
	t.b.SetDeadline(at)
}

// copy copies src to dst until either ends or the tunnel goes idle.
func (t *tunnel) copy(dst io.Writer, src io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			t.touch()
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

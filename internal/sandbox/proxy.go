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
// GitHub, and the Go, npm and PyPI registries.
var DefaultAllowed = []string{
	"github.com", "api.github.com", "codeload.github.com", "*.githubusercontent.com",
	"proxy.golang.org", "sum.golang.org", "dl.google.com",
	"registry.npmjs.org",
	"pypi.org", "files.pythonhosted.org",
}

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

	mu   sync.Mutex
	open map[string]int // desk's address -> tunnels open
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

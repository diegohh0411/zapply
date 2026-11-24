package edgeworker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"time"
)

// guardedProxy is the only way out for the headless browser. Chrome is started
// with --proxy-server pointing here, so every connection a page opens (HTTP,
// HTTPS, WebSocket, redirects, sub-resources) is dialled by Go through
// Guard.DialControl, which checks the address really being connected to. This
// closes what request interception alone cannot: a name that answers with a
// public address when checked and a private one when Chrome connects (DNS
// rebinding), and traffic the DevTools Fetch domain never pauses, such as
// WebSocket handshakes.
type guardedProxy struct {
	ln     net.Listener
	srv    *http.Server
	dialer *net.Dialer
	guard  *Guard
	log    *slog.Logger
	wg     sync.WaitGroup

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{} // open tunnel ends, closed on Close
}

// startGuardedProxy listens on a random loopback port.
func startGuardedProxy(g *Guard, log *slog.Logger) (*guardedProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &guardedProxy{
		ln:     ln,
		dialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: g.DialControl},
		guard:  g,
		log:    log,
		conns:  map[net.Conn]struct{}{},
	}
	forward := &httputil.ReverseProxy{
		// The request is already in absolute form (http://host/path); keep it.
		Rewrite: func(r *httputil.ProxyRequest) { r.Out.URL = r.In.URL; r.Out.Host = r.In.Host },
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           p.dialGuarded,
			MaxIdleConns:          32,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	p.srv = &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				p.tunnel(w, r)
				return
			}
			if r.URL.Scheme != "http" || r.URL.Host == "" {
				http.Error(w, "proxy requests only", http.StatusBadRequest)
				return
			}
			if !p.allowedPort(r.URL.Port(), "80") {
				http.Error(w, "port refused", http.StatusForbidden)
				return
			}
			// ReverseProxy also carries Upgrade requests (plain ws://) through.
			forward.ServeHTTP(w, r)
		}),
	}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// URL is the value for Chrome's --proxy-server flag.
func (p *guardedProxy) URL() string { return "http://" + p.ln.Addr().String() }

// Close stops the listener and every open tunnel.
func (p *guardedProxy) Close() {
	_ = p.srv.Close()
	p.mu.Lock()
	p.closed = true
	for c := range p.conns {
		_ = c.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
}

// allowedPort keeps the browser on the web's ports, as CheckURL does.
func (p *guardedProxy) allowedPort(port, def string) bool {
	return p.guard.allowPrivate || port == "" || port == def || port == "80" || port == "443"
}

func (p *guardedProxy) dialGuarded(ctx context.Context, network, addr string) (net.Conn, error) {
	return p.dialer.DialContext(ctx, network, addr)
}

// tunnel serves CONNECT, which Chrome uses for https:// and wss://.
func (p *guardedProxy) tunnel(w http.ResponseWriter, r *http.Request) {
	_, port, err := net.SplitHostPort(r.Host)
	if err != nil || !p.allowedPort(port, "443") {
		http.Error(w, "port refused", http.StatusForbidden)
		return
	}
	upstream, err := p.dialGuarded(r.Context(), "tcp", r.Host)
	if err != nil {
		p.log.Debug("edge proxy refused", "host", r.Host, "err", err)
		http.Error(w, fmt.Sprintf("dial refused: %v", err), http.StatusForbidden)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	p.conns[client], p.conns[upstream] = struct{}{}, struct{}{}
	p.wg.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.wg.Done()
		defer func() {
			p.mu.Lock()
			delete(p.conns, client)
			delete(p.conns, upstream)
			p.mu.Unlock()
		}()
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(upstream, buf); done <- struct{}{} }()
		go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
		<-done
		_ = client.Close()
		_ = upstream.Close()
		<-done
	}()
}

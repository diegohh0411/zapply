package edgeworker

import (
	"bufio"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func proxiedClient(t *testing.T, g *Guard) *http.Client {
	t.Helper()
	p, err := startGuardedProxy(g, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	u, _ := url.Parse(p.URL())
	return &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(u),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // httptest certificate
	}}
}

// The proxy dials by address, so a name that passed CheckURL but now points at
// loopback (DNS rebinding) is still refused, for http and for CONNECT.
func TestGuardedProxyRefusesPrivateUpstreams(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "secret") }))
	defer plain.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "secret") }))
	defer tlsSrv.Close()

	client := proxiedClient(t, &Guard{Resolver: fakeResolver{}})
	for _, target := range []string{
		plain.URL,
		tlsSrv.URL,
	} {
		resp, err := client.Get(target)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK || strings.Contains(string(body), "secret") {
				t.Fatalf("%s: reached a loopback upstream through the proxy (%d %q)", target, resp.StatusCode, body)
			}
		}
	}
}

func TestGuardedProxyForwardsHTTPAndConnect(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "plain "+r.URL.Path) }))
	defer plain.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "tls") }))
	defer tlsSrv.Close()

	client := proxiedClient(t, &Guard{allowPrivate: true})
	for target, want := range map[string]string{plain.URL + "/x": "plain /x", tlsSrv.URL: "tls"} {
		resp, err := client.Get(target)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != want {
			t.Fatalf("%s: got %q, want %q", target, body, want)
		}
	}
}

// A plain ws:// handshake is an Upgrade request; it must be dialled through
// the guard too, so it is refused for loopback.
func TestGuardedProxyRefusesWebSocketUpgradeToPrivate(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	reached := make(chan struct{}, 1)
	go func() {
		if c, err := upstream.Accept(); err == nil {
			reached <- struct{}{}
			_ = c.Close()
		}
	}()

	p, err := startGuardedProxy(&Guard{Resolver: fakeResolver{}}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	conn, err := net.Dial("tcp", p.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	addr := upstream.Addr().String()
	_, _ = io.WriteString(conn, "GET http://"+addr+"/ws HTTP/1.1\r\nHost: "+addr+"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("upgrade to a loopback address went through")
	}
	select {
	case <-reached:
		t.Fatal("the proxy connected to the loopback upstream")
	default:
	}
}

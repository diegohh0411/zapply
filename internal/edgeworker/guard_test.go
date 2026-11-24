package edgeworker

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

type fakeResolver map[string][]string

func (r fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	ips, ok := r[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	var out []netip.Addr
	for _, s := range ips {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

func TestGuardCheckURL(t *testing.T) {
	g := &Guard{Resolver: fakeResolver{
		"www.linkedin.com": {"13.107.42.14", "2620:1ec:21::14"},
		"rebind.example":   {"93.184.216.34", "10.0.0.5"},
		"internal.example": {"192.168.1.20"},
		"v4mapped.example": {"::ffff:127.0.0.1"},
		"cgnat.example":    {"100.100.100.100"},
		"nat64.example":    {"64:ff9b::7f00:1"},
	}}
	for _, tc := range []struct {
		url  string
		want bool // refused
	}{
		{"https://www.linkedin.com/jobs/view/1", false},
		{"http://www.linkedin.com/jobs/view/1", false},
		{"https://www.linkedin.com:443/jobs/view/1", false},
		{"https://WWW.LinkedIn.com./jobs/view/1", false},
		{"https://93.184.216.34/", false},
		{"file:///etc/passwd", true},
		{"ftp://www.linkedin.com/", true},
		{"javascript:alert(1)", true},
		{"https://user:pw@www.linkedin.com/", true},
		{"https://www.linkedin.com:8443/", true},
		{"http://127.0.0.1/", true},
		{"http://127.0.0.1:80/", true},
		{"http://[::1]/", true},
		{"http://0.0.0.0/", true},
		{"http://10.1.2.3/", true},
		{"http://172.16.0.1/", true},
		{"http://192.168.0.1/", true},
		{"http://169.254.169.254/latest/meta-data/", true},
		{"http://[fe80::1]/", true},
		{"http://[fd00::1]/", true},
		{"http://100.64.0.1/", true},
		{"http://[::ffff:10.0.0.1]/", true},
		{"http://224.0.0.1/", true},
		{"http://localhost/", true},
		{"http://app.localhost/", true},
		{"http://printer.local/", true},
		{"http://nas.home.arpa/", true},
		{"https://internal.example/", true},
		{"https://rebind.example/", true}, // one private answer is enough
		{"https://v4mapped.example/", true},
		{"https://cgnat.example/", true},
		{"https://nat64.example/", true},
		{"https://unknown.example/", true}, // does not resolve
		{"", true},
	} {
		err := g.CheckURL(t.Context(), tc.url)
		if (err != nil) != tc.want {
			t.Errorf("CheckURL(%q) = %v, refused = %v, want %v", tc.url, err, err != nil, tc.want)
		}
	}
}

func TestGuardRefusesPrivateAddressesWhenDialling(t *testing.T) {
	g := NewGuard()
	for addr, want := range map[string]bool{
		"93.184.216.34:443":                            false,
		"[2606:2800:21f:cb07:6820:80da:af6b:8b2c]:443": false,
		"127.0.0.1:443":                                true,
		"10.0.0.8:80":                                  true,
		"[::1]:443":                                    true,
		"169.254.169.254:80":                           true,
		"not-an-ip:80":                                 true,
	} {
		if err := g.DialControl("tcp", addr, nil); (err != nil) != want {
			t.Errorf("DialControl(%s) = %v, refused = %v, want %v", addr, err, err != nil, want)
		}
	}
}

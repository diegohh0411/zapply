// Package joburl defines posting URL identity for writes and data migrations.
package joburl

import (
	"errors"
	"net/url"
	"strings"
)

// trackingParams are dropped when normalizing a posting URL, so the same
// posting shared through different channels is recognized.
var trackingParams = map[string]bool{
	"gclid": true, "fbclid": true, "trk": true, "trkinfo": true, "refid": true,
	"trackingid": true, "src": true, "ref": true, "source": true, "mc_cid": true, "mc_eid": true,
}

// Normalize is the duplicate key for posting URLs: scheme and host lower
// case, default port, fragment, tracking parameters and trailing slash
// removed, remaining parameters sorted.
func Normalize(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", errors.New("not an http(s) URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if p := u.Port(); p != "" && !(u.Scheme == "http" && p == "80") && !(u.Scheme == "https" && p == "443") {
		host += ":" + p
	}
	u.Host = host
	u.User = nil
	u.Fragment, u.RawFragment = "", ""
	q := u.Query()
	for k := range q {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "utm_") || trackingParams[lk] {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	// Trim literal separators without decoding escaped separators such as %2F.
	escaped := strings.TrimRight(u.EscapedPath(), "/")
	u.Path, err = url.PathUnescape(escaped)
	if err != nil {
		return "", err
	}
	u.RawPath = escaped
	return u.String(), nil
}

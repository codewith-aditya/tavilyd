package search

import (
	"net/url"
	"strings"
)

// trackingParams are removed during canonicalization so that the same article
// shared with different campaign tags dedupes to one entry.
var trackingParams = map[string]struct{}{
	"utm_source": {}, "utm_medium": {}, "utm_campaign": {}, "utm_term": {}, "utm_content": {},
	"utm_id": {}, "utm_name": {}, "utm_reader": {}, "utm_referrer": {}, "utm_social": {},
	"gclid": {}, "dclid": {}, "fbclid": {}, "msclkid": {}, "yclid": {}, "twclid": {},
	"igshid": {}, "mc_cid": {}, "mc_eid": {}, "_hsenc": {}, "_hsmi": {}, "vero_id": {},
	"ref": {}, "ref_src": {}, "ref_url": {}, "source": {}, "spm": {}, "s_cid": {},
	"cmpid": {}, "cid": {}, "mkt_tok": {}, "oly_anon_id": {}, "oly_enc_id": {},
}

// Canonical returns a normalized absolute URL used as the dedupe key. It strips
// fragments, default ports, tracking parameters and the "www." prefix.
func Canonical(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", &url.Error{Op: "canonical", URL: raw, Err: errEmptyHost}
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(strings.TrimSuffix(host, "."), "www.")
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		u.Host = host + ":" + port
	} else {
		u.Host = host
	}
	u.Fragment = ""
	u.RawFragment = ""

	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	u.RawPath = ""
	u.Path = path

	if u.RawQuery != "" {
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			u.RawQuery = ""
		} else {
			for k := range q {
				if _, drop := trackingParams[strings.ToLower(k)]; drop {
					delete(q, k)
				}
			}
			u.RawQuery = q.Encode()
		}
	}
	return u.String(), nil
}

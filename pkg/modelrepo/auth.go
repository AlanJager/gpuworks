package modelrepo

import (
	"encoding/base64"
	"net/url"
	"strings"
)

// Login is a stored login for one host: a token when Username is empty (sent
// as a Bearer token, as Hugging Face takes it), else a user name and password
// (sent as Basic).
type Login struct {
	Host     string
	Username string
	Secret   string
}

// AuthorizationFor is the Authorization header a request to rawURL carries,
// or "" for none. A login applies to its own host only - a mirror in front of
// a hub never receives the hub's token - and only over https: a token sent in
// clear to a plain-HTTP proxy someone listed by mistake is a token given
// away. This is the one rule for every model request, a node's downloads and
// the management node's metadata reads alike. Redirects are left to the HTTP
// client, which forwards the header to the same domain and its subdomains
// only (a hub's CDN) and never to another domain (a signed storage URL).
func AuthorizationFor(rawURL string, logins []Login) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") {
		return ""
	}
	host := hostKey(parsed.Host)
	for _, login := range logins {
		if login.Secret == "" || hostKey(login.Host) != host {
			continue
		}
		if login.Username == "" {
			return "Bearer " + login.Secret
		}
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(login.Username+":"+login.Secret))
	}
	return ""
}

// AuthorizationHeaders is AuthorizationFor as request headers: nil for none.
func AuthorizationHeaders(rawURL string, logins []Login) map[string]string {
	if header := AuthorizationFor(rawURL, logins); header != "" {
		return map[string]string{"Authorization": header}
	}
	return nil
}

// hostKey spells a host the way logins are compared: lower case, without the
// https default port.
func hostKey(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	return strings.TrimSuffix(host, ":443")
}

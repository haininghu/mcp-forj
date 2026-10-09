package gitproxy

import (
	"crypto/subtle"
	"net/http"
)

// authChallenge advertises HTTP Basic so git clients retry with credentials.
const authChallenge = `Basic realm="mcp-forj git proxy", charset="UTF-8"`

// authenticate verifies HTTP Basic auth. The username is ignored (git sends
// whatever the credential helper holds); the password must equal the
// configured token. The comparison is constant-time and neither the token nor
// the submitted password is ever logged.
func (s *Server) authenticate(r *http.Request) bool {
	_, password, ok := r.BasicAuth()
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(password), []byte(s.cfg.Token)) == 1
}

// setAuthChallenge adds the WWW-Authenticate header to a 401 response.
func setAuthChallenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", authChallenge)
}

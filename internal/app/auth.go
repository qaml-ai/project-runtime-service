package app

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

func (s *Server) rejectUnauthenticatedControlRequest(w http.ResponseWriter, req *http.Request) bool {
	authType := strings.ToLower(strings.TrimSpace(s.cfg.ControlAuthType))
	switch authType {
	case "", "none":
		return false
	case "bearer":
		expected := strings.TrimSpace(s.cfg.ControlBearerToken)
		if expected == "" {
			errorJSON(w, "Control-plane bearer auth is not configured", http.StatusServiceUnavailable)
			return true
		}
		actual := strings.TrimSpace(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		if actual == "" || subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
			errorJSON(w, "Unauthorized", http.StatusUnauthorized)
			return true
		}
		return false
	case "mtls":
		if req.TLS == nil || len(req.TLS.PeerCertificates) == 0 {
			errorJSON(w, "Client certificate required", http.StatusUnauthorized)
			return true
		}
		return false
	default:
		errorJSON(w, "Unsupported control-plane auth type", http.StatusServiceUnavailable)
		return true
	}
}

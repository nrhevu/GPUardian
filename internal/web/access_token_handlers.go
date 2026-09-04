package web

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

type createAccessTokenRequest struct {
	Name       string   `json:"name"`
	Scopes     []string `json:"scopes"`
	ExpiryDays int      `json:"expiry_days"`
}

func (s *Server) handleAccessTokens(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/access-tokens"), "/")
	if rest != "" {
		if r.Method != http.MethodDelete || strings.Contains(rest, "/") {
			writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
		if err := s.Users.RevokeAccessToken(session.User, rest); err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	switch r.Method {
	case http.MethodGet:
		tokens, err := s.Users.ListAccessTokens(session.User)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"tokens": tokens, "supported_scopes": sortedAccessTokenSupportedScopes(), "default_scopes": append([]string(nil), accessTokenDefaultScopes...),
		})
	case http.MethodPost:
		var req createAccessTokenRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.ExpiryDays == 0 {
			req.ExpiryDays = 30
		}
		if req.ExpiryDays < 1 || req.ExpiryDays > accessTokenMaxDays {
			writeJSONError(w, http.StatusBadRequest, "expiry_days must be between 1 and 365")
			return
		}
		token, err := s.Users.CreateAccessToken(
			session.User, req.Name, req.Scopes, time.Now().UTC().Add(time.Duration(req.ExpiryDays)*24*time.Hour),
		)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, token)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func sortedAccessTokenSupportedScopes() []string {
	out := make([]string, 0, len(accessTokenSupportedScopes))
	for scope := range accessTokenSupportedScopes {
		out = append(out, scope)
	}
	sort.Strings(out)
	return out
}

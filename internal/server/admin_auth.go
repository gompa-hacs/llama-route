package server

import (
	"encoding/json"
	"net/http"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

func (s *Server) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeJSON(w, http.StatusOK, map[string]any{"adminRequired": false, "authenticated": true, "inferenceRequired": false})
		return
	}

	authed := !s.auth.AdminRequired() || s.auth.SessionValid(sessionToken(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"adminRequired":     s.auth.AdminRequired(),
		"authenticated":     authed,
		"inferenceRequired": s.auth.InferenceRequired(),
	})
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || !s.auth.AdminRequired() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "admin login is not enabled"})
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		shared.SendResponse(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}

	token, exp, err := s.auth.Login(req.Password)
	if err != nil {
		shared.SendResponse(w, r, http.StatusUnauthorized, "invalid credentials")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  exp,
	})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "expires": exp.UTC()})
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if s.auth != nil {
		s.auth.Logout(sessionToken(r))
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminListKeys(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeJSON(w, http.StatusOK, map[string]any{"keys": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": s.auth.ListKeys()})
}

func (s *Server) handleAdminCreateKey(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		shared.SendResponse(w, r, http.StatusServiceUnavailable, "key management unavailable")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		shared.SendResponse(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}
	pub, secret, err := s.auth.CreateKey(req.Name)
	if err != nil {
		shared.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": pub, "secret": secret})
}

func (s *Server) handleAdminRevokeKey(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		shared.SendResponse(w, r, http.StatusServiceUnavailable, "key management unavailable")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		shared.SendResponse(w, r, http.StatusBadRequest, "missing key id")
		return
	}
	if err := s.auth.RevokeKey(id); err != nil {
		shared.SendResponse(w, r, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func sessionToken(r *http.Request) string {
	c, err := r.Cookie(auth.SessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

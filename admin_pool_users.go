package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

func passportClientAsPoolUser(h *proxyHandler, client *ClientCredential) *PoolUser {
	if client == nil {
		return nil
	}
	if dl, err := h.passport.clientDownloadToken(client); err == nil {
		client.DownloadToken = dl
	}
	if client.DownloadToken == "" {
		return nil
	}
	principalID, clientID, ok := h.passport.authorizeCredential(client.PrincipalID + "-c-" + client.ID)
	if !ok {
		return nil
	}
	pr := h.passport.principal(principalID)
	issuedAt := time.Now().UTC()
	if pr.CredentialsValidAfter.After(issuedAt) {
		issuedAt = pr.CredentialsValidAfter
	}
	if client.ValidAfter.After(issuedAt) {
		issuedAt = client.ValidAfter
	}
	return &PoolUser{ID: principalID + "-c-" + clientID, Token: client.DownloadToken, Email: pr.Email, PlanType: pr.PlanType, CreatedAt: client.CreatedAt, credentialIssuedAt: issuedAt}
}

// Config download endpoints (no auth - token IS the auth)

func (h *proxyHandler) serveConfigDownload(w http.ResponseWriter, r *http.Request) {
	if h.passport == nil {
		respondJSONError(w, http.StatusServiceUnavailable, "pool identities not configured")
		return
	}

	path := r.URL.Path
	var configType string
	var token string

	switch {
	case strings.HasPrefix(path, "/config/codex/"):
		configType = "codex"
		token = strings.TrimPrefix(path, "/config/codex/")
	case strings.HasPrefix(path, "/config/gemini/"):
		configType = "gemini"
		token = strings.TrimPrefix(path, "/config/gemini/")
	case strings.HasPrefix(path, "/config/antigravity/"):
		configType = "antigravity"
		token = strings.TrimPrefix(path, "/config/antigravity/")
	case strings.HasPrefix(path, "/config/claude/"):
		configType = "claude"
		token = strings.TrimPrefix(path, "/config/claude/")
	case strings.HasPrefix(path, "/config/pi/"):
		configType = "pi"
		token = strings.TrimPrefix(path, "/config/pi/")
	case strings.HasPrefix(path, "/config/grok/"):
		configType = "grok"
		token = strings.TrimPrefix(path, "/config/grok/")
	default:
		http.NotFound(w, r)
		return
	}

	token = strings.TrimSuffix(token, "/")
	if token == "" {
		respondJSONError(w, http.StatusBadRequest, "token required")
		return
	}

	var user *PoolUser
	if client := h.passport.redeemConfigDownloadNonce(token); client != nil {
		user = passportClientAsPoolUser(h, client)
	}
	if user == nil {
		respondJSONError(w, http.StatusNotFound, "invalid or revoked token")
		return
	}
	if user.Disabled {
		respondJSONError(w, http.StatusForbidden, "user disabled")
		return
	}

	secret := getPoolJWTSecret()
	if secret == "" {
		respondJSONError(w, http.StatusServiceUnavailable, "JWT secret not configured")
		return
	}

	w.Header().Set("Content-Type", "application/json")

	switch configType {
	case "codex":
		auth, err := generateCodexAuth(secret, user)
		if err != nil {
			respondJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		json.NewEncoder(w).Encode(auth)
	case "gemini":
		auth, err := generateGeminiAuth(secret, user)
		if err != nil {
			respondJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		json.NewEncoder(w).Encode(auth)
	case "antigravity":
		apiKey := generateGeminiAPIKey(secret, user)
		baseURL := strings.TrimRight(h.getEffectivePublicURL(r), "/")
		respondJSON(w, map[string]any{
			"api_key":       apiKey,
			"base_url":      baseURL,
			"settings_file": "~/.gemini/antigravity-cli/settings.json",
			"settings":      map[string]any{"modelProvider": "gemini"},
			"env": map[string]string{
				"GEMINI_API_KEY":         apiKey,
				"GOOGLE_GEMINI_BASE_URL": baseURL,
			},
		})
	case "claude":
		auth, err := generateClaudeAuth(secret, user)
		if err != nil {
			respondJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		json.NewEncoder(w).Encode(auth)
	case "pi":
		codexAuth, err := generateCodexAuth(secret, user)
		if err != nil {
			respondJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		auth, err := generateClaudeAuth(secret, user)
		if err != nil {
			respondJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		publicURL := h.getEffectivePublicURL(r)
		codexAccessToken := ""
		if codexAuth.Tokens != nil {
			codexAccessToken = codexAuth.Tokens.AccessToken
		}
		modelsJSON, err := generatePiModelsJSON(publicURL, codexAccessToken, auth.AccessToken)
		if err != nil {
			respondJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Write(modelsJSON)
	case "grok":
		auth, err := generateClaudeAuth(secret, user)
		if err != nil {
			respondJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondJSON(w, map[string]any{
			"api_key":        auth.AccessToken,
			"base_url":       strings.TrimRight(h.getEffectivePublicURL(r), "/") + "/v1",
			"model":          "grok-4.5",
			"api_backend":    "responses",
			"context_window": 500000,
		})
	}
}

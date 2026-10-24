package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.etcd.io/bbolt"
)

const appTokenIssuer = "codex-pool"
const appTokenAudience = "codex-pool-app"
const appClientPurpose = "app"
const appTokenLifetime = 30 * 24 * time.Hour

var errAppClientLimit = errors.New("client credential limit reached")

type appTokenClaims struct {
	ClientID          string `json:"client_id"`
	PasswordChangedAt string `json:"password_changed_at"`
	jwt.RegisteredClaims
}

type appLoginResponse struct {
	Token     string    `json:"token"`
	ClientID  string    `json:"client_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// The credential and its audit entry commit together, after revalidating the
// account that passed password verification. No browser session is created.
func (p *PassportStore) createAppToken(verified *Principal, secret string) (*appLoginResponse, error) {
	if secret == "" {
		return nil, errors.New("app token signing unavailable")
	}
	id, err := secureID(9)
	if err != nil {
		return nil, err
	}
	download, err := secureToken(24)
	if err != nil {
		return nil, err
	}
	sealed, err := p.seal("client", id, verified.ID, download)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var result appLoginResponse
	var client ClientCredential
	err = p.db.Update(func(tx *bbolt.Tx) error {
		var principal Principal
		raw := tx.Bucket([]byte(bucketPrincipals)).Get([]byte(verified.ID))
		if raw == nil || json.Unmarshal(raw, &principal) != nil {
			return errors.New("account unavailable")
		}
		now := time.Now().UTC()
		if principal.PasswordHash != verified.PasswordHash || principal.Status != PrincipalActive ||
			(principal.Kind != PrincipalMember && principal.Kind != PrincipalOperator) ||
			(principal.ExpiresAt != nil && !principal.ExpiresAt.After(now)) ||
			(principal.Kind == PrincipalOperator && !principalIsUsableOperator(&principal)) {
			return errors.New("account changed during sign in")
		}
		bucket := tx.Bucket([]byte(bucketClientCredentials))
		count := 0
		if err := bucket.ForEach(func(_, value []byte) error {
			var existing ClientCredential
			if err := json.Unmarshal(value, &existing); err != nil {
				return err
			}
			if existing.PrincipalID == principal.ID && existing.Status == "active" {
				count++
			}
			return nil
		}); err != nil {
			return err
		}
		if count >= 20 {
			return errAppClientLimit
		}
		expires := now.Add(appTokenLifetime).Truncate(time.Second)
		if principal.ExpiresAt != nil && principal.ExpiresAt.Before(expires) {
			expires = principal.ExpiresAt.Truncate(time.Second)
		}
		if !expires.After(now) {
			return errors.New("account is expiring")
		}
		digest := hashToken(download)
		client = ClientCredential{ID: id, PrincipalID: principal.ID, Label: "codex-pool app", Purpose: appClientPurpose,
			Status: "active", ExpiresAt: &expires, DownloadDigest: hex.EncodeToString(digest[:]),
			DownloadCiphertext: sealed, CreatedAt: now}
		claims := appTokenClaims{ClientID: id, PasswordChangedAt: principal.PasswordChangedAt.UTC().Format(time.RFC3339Nano),
			RegisteredClaims: jwt.RegisteredClaims{Issuer: appTokenIssuer, Subject: principal.ID,
				Audience: jwt.ClaimStrings{appTokenAudience}, IssuedAt: jwt.NewNumericDate(now),
				NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(expires), ID: id}}
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
		if err != nil {
			return err
		}
		if err := putJSON(bucket, id, &client); err != nil {
			return err
		}
		if err := p.audit(tx, principal.ID, "app.login", id, ""); err != nil {
			return err
		}
		result = appLoginResponse{Token: token, ClientID: id, ExpiresAt: expires}
		return nil
	})
	if err != nil {
		return nil, err
	}
	p.clients[id] = &client
	return &result, nil
}

func (h *proxyHandler) authenticateAppRequest(r *http.Request) (*Principal, *ClientCredential) {
	if h.passport == nil || len(r.Header.Values("Authorization")) != 1 {
		return nil, nil
	}
	header := r.Header.Get("Authorization")
	secret := getPoolJWTSecret()
	if secret == "" || !strings.HasPrefix(header, "Bearer ") {
		return nil, nil
	}
	var claims appTokenClaims
	token, err := jwt.ParseWithClaims(strings.TrimPrefix(header, "Bearer "), &claims,
		func(*jwt.Token) (any, error) { return []byte(secret), nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(appTokenIssuer), jwt.WithAudience(appTokenAudience),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil || !token.Valid || claims.IssuedAt == nil || claims.Subject == "" || claims.ClientID == "" {
		return nil, nil
	}
	identity := claims.Subject + "-c-" + claims.ClientID
	principal, client, ok := h.passport.credentialState(identity)
	if !ok || client.Purpose != appClientPurpose || claims.PasswordChangedAt != principal.PasswordChangedAt.UTC().Format(time.RFC3339Nano) {
		return nil, nil
	}
	if _, _, allowed := h.passport.authorizeIssuedCredential(identity, claims.IssuedAt.Time); !allowed {
		return nil, nil
	}
	h.passport.markCredentialSeen(principal.ID, client.ID, time.Now())
	return principal, client
}

// An explicitly supplied app bearer is the sole authority for that request.
// Browser cookies are considered only when no Authorization header is present.
func (h *proxyHandler) authenticateMemberRequest(r *http.Request) (*Principal, *passportSession, string) {
	if len(r.Header.Values("Authorization")) != 0 {
		principal, client := h.authenticateAppRequest(r)
		if principal == nil {
			return nil, nil, ""
		}
		return principal, nil, principal.ID + "-c-" + client.ID
	}
	if h.passport == nil {
		return nil, nil, ""
	}
	principal, session := h.passport.authenticate(r)
	if principal == nil {
		return nil, nil, ""
	}
	return principal, session, principal.ID
}

func (h *proxyHandler) handleAppToken(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	switch r.Method {
	case http.MethodPost:
		secret := getPoolJWTSecret()
		if secret == "" {
			respondJSONError(w, http.StatusServiceUnavailable, "app token signing unavailable")
			return
		}
		principal := h.passwordLoginPrincipal(w, r)
		if principal == nil {
			return
		}
		result, err := h.passport.createAppToken(principal, secret)
		if err != nil {
			if errors.Is(err, errAppClientLimit) {
				respondJSONError(w, http.StatusConflict, "client credential limit reached")
			} else {
				respondJSONError(w, http.StatusServiceUnavailable, "app credential issuance unavailable")
			}
			return
		}
		h.passwordLoginSucceeded(r)
		respondJSON(w, result)
	case http.MethodDelete:
		principal, client := h.authenticateAppRequest(r)
		if principal == nil {
			respondJSONError(w, http.StatusUnauthorized, "app authentication required")
			return
		}
		if err := h.passport.revokeClient(principal.ID, principal.ID, client.ID); err != nil {
			respondJSONError(w, http.StatusServiceUnavailable, "app credential revocation unavailable")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "POST, DELETE")
		respondJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *proxyHandler) handleAppCatalog(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		respondJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	principal, client := h.authenticateAppRequest(r)
	if principal == nil {
		respondJSONError(w, http.StatusUnauthorized, "app authentication required")
		return
	}
	if h.pool == nil || h.pool.accountAuthority != h.passport || h.cfg == nil {
		respondJSONError(w, http.StatusServiceUnavailable, "account authority unavailable")
		return
	}
	visible, err := h.memberQuotaSnapshot(principal.ID+"-c-"+client.ID, time.Now().UTC())
	if err != nil {
		respondJSONError(w, http.StatusServiceUnavailable, "catalog authorization unavailable")
		return
	}
	serveClientPoolModels(w, visible)
}

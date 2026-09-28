package main

import (
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

const bucketMemberRecoveryLinks = "member_recovery_links"

type memberRecoveryLink struct {
	ID          string    `json:"id"`
	PrincipalID string    `json:"principal_id"`
	Purpose     string    `json:"purpose"`
	TokenDigest string    `json:"token_digest"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type memberLinkResult struct {
	Principal *Principal
	Token     string
	ExpiresAt time.Time
}

func copyPrincipal(principal *Principal) *Principal {
	if principal == nil {
		return nil
	}
	copy := *principal
	copy.WebAuthnUserID = append([]byte(nil), principal.WebAuthnUserID...)
	return &copy
}

func normalizeUsername(value string) (string, error) {
	username := strings.ToLower(strings.TrimSpace(value))
	if len(username) < 3 || len(username) > 32 {
		return "", errors.New("username must be 3 to 32 characters")
	}
	for _, char := range username {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.' {
			continue
		}
		return "", errors.New("username may use letters, numbers, dots, dashes, and underscores")
	}
	return username, nil
}

func normalizeMemberEmail(value string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(value))
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email || !strings.Contains(email, "@") {
		return "", errors.New("valid member email required")
	}
	return email, nil
}

func (p *PassportStore) createMemberLink(actorID, email, displayName, purpose string) (*memberLinkResult, error) {
	email, err := normalizeMemberEmail(email)
	if err != nil {
		return nil, err
	}
	if purpose != "onboard" && purpose != "recover" {
		return nil, errors.New("invalid member link purpose")
	}
	displayName = strings.TrimSpace(displayName)
	if len([]rune(displayName)) > 48 {
		return nil, errors.New("display name must be 48 characters or fewer")
	}

	principal := p.byEmail(email)
	if purpose == "onboard" {
		if principal != nil {
			return nil, errors.New("an account already uses that email")
		}
		id, err := secureID(12)
		if err != nil {
			return nil, err
		}
		principal = &Principal{
			ID: id, Kind: PrincipalMember, Status: PrincipalActive,
			DisplayName: strings.TrimSpace(displayName), Email: email,
			Note: "member", CreatedAt: time.Now().UTC(),
		}
	} else if principal == nil || principal.Kind != PrincipalMember {
		return nil, errors.New("member not found")
	}

	id, err := secureID(9)
	if err != nil {
		return nil, err
	}
	token, err := secureToken(32)
	if err != nil {
		return nil, err
	}
	digest := hashToken(token)
	now := time.Now().UTC()
	link := memberRecoveryLink{
		ID: id, PrincipalID: principal.ID, Purpose: purpose,
		TokenDigest: hex.EncodeToString(digest[:]), CreatedBy: actorID,
		CreatedAt: now, ExpiresAt: now.Add(30 * time.Minute),
	}

	err = p.db.Update(func(tx *bbolt.Tx) error {
		principals := tx.Bucket([]byte(bucketPrincipals))
		links := tx.Bucket([]byte(bucketMemberRecoveryLinks))
		if purpose == "onboard" {
			// Transactional email uniqueness: the pre-transaction byEmail check
			// races under concurrent onboarding, the bucket scan cannot.
			duplicate := false
			if err := principals.ForEach(func(_, value []byte) error {
				var existing Principal
				if json.Unmarshal(value, &existing) == nil && strings.EqualFold(existing.Email, email) && existing.Email != "" {
					duplicate = true
				}
				return nil
			}); err != nil {
				return err
			}
			if duplicate {
				return errors.New("an account already uses that email")
			}
			if err := putJSON(principals, principal.ID, principal); err != nil {
				return err
			}
		} else {
			var current Principal
			if value := principals.Get([]byte(principal.ID)); value == nil || json.Unmarshal(value, &current) != nil || current.Kind != PrincipalMember {
				return errors.New("member not found")
			}
			// A fresh recovery link retires every pending link of this principal.
			if err := deleteMemberLinksForPrincipal(links, principal.ID); err != nil {
				return err
			}
		}
		if err := putJSON(links, link.ID, &link); err != nil {
			return err
		}
		action := "member.onboarding_link_created"
		if purpose == "recover" {
			action = "member.recovery_link_created"
		}
		return p.audit(tx, actorID, action, principal.ID, email)
	})
	if err != nil {
		return nil, err
	}
	if purpose == "onboard" {
		p.mu.Lock()
		p.principals[principal.ID] = principal
		p.mu.Unlock()
	}
	return &memberLinkResult{Principal: copyPrincipal(principal), Token: token, ExpiresAt: link.ExpiresAt}, nil
}

func (p *PassportStore) memberLinkByDigest(digest [32]byte) (memberRecoveryLink, bool) {
	var link memberRecoveryLink
	found := false
	_ = p.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(bucketMemberRecoveryLinks)).ForEach(func(_, value []byte) error {
			var candidate memberRecoveryLink
			if json.Unmarshal(value, &candidate) != nil {
				return nil
			}
			stored, decodeErr := hex.DecodeString(candidate.TokenDigest)
			if decodeErr == nil && len(stored) == len(digest) && subtle.ConstantTimeCompare(stored, digest[:]) == 1 {
				link = candidate
				found = true
			}
			return nil
		})
	})
	return link, found
}

// memberLinkExpired applies the strict recovery deadline: a link is dead once
// now >= ExpiresAt. A missing expiry is treated as expired, never as open.
func memberLinkExpired(link memberRecoveryLink, now time.Time) bool {
	return link.ExpiresAt.IsZero() || !now.Before(link.ExpiresAt)
}

// memberLinkUsable is the single validity predicate shared by the status
// preflight and the redeem path: the link must be live and its principal an
// active, unexpired member.
func memberLinkUsable(link memberRecoveryLink, principal *Principal, now time.Time) bool {
	if link.PrincipalID == "" || link.TokenDigest == "" {
		return false
	}
	if memberLinkExpired(link, now) {
		return false
	}
	if principal == nil || principal.Kind != PrincipalMember || principal.Status != PrincipalActive {
		return false
	}
	if principal.ExpiresAt != nil && !now.Before(*principal.ExpiresAt) {
		return false
	}
	return true
}

// memberLinkStatus answers whether a recovery token may still be redeemed.
// It never hashes passwords: unknown tokens only cost a digest comparison.
func (p *PassportStore) memberLinkStatus(token string) (memberRecoveryLink, bool) {
	link, found := p.memberLinkByDigest(hashToken(strings.TrimSpace(token)))
	if !found {
		return link, false
	}
	return link, memberLinkUsable(link, p.principal(link.PrincipalID), time.Now().UTC())
}

// cleanupExpiredMemberLinks lazily reclaims dead links. Keys are collected as
// copies and deleted after the iteration, never inside ForEach. Expiry
// validation stays mandatory regardless of cleanup.
func (p *PassportStore) cleanupExpiredMemberLinks(now time.Time) {
	_ = p.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketMemberRecoveryLinks))
		var keys [][]byte
		if err := bucket.ForEach(func(key, value []byte) error {
			var candidate memberRecoveryLink
			if json.Unmarshal(value, &candidate) == nil && memberLinkExpired(candidate, now) {
				keys = append(keys, append([]byte(nil), key...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, key := range keys {
			if err := bucket.Delete(key); err != nil {
				return err
			}
		}
		return nil
	})
}

func (p *PassportStore) redeemMemberLink(token, password string) (*Principal, string, string, error) {
	if len(password) < 12 {
		return nil, "", "", errors.New("password must be at least 12 characters")
	}
	// Cheap digest lookup first: invalid tokens must never reach Argon2.
	digest := hashToken(strings.TrimSpace(token))
	link, found := p.memberLinkByDigest(digest)
	now := time.Now().UTC()
	if !found || !memberLinkUsable(link, p.principal(link.PrincipalID), now) {
		return nil, "", "", errors.New("member link unavailable")
	}
	select {
	case p.passwordWork <- struct{}{}:
		defer func() { <-p.passwordWork }()
	default:
		return nil, "", "", errors.New("password verification busy")
	}
	passwordHash, err := hashPassword(password)
	if err != nil {
		return nil, "", "", err
	}
	var updated Principal
	err = p.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketMemberRecoveryLinks))
		// Re-check the link inside the transaction: it may have been consumed
		// between the pre-check and this write.
		var live memberRecoveryLink
		matched := false
		if err := bucket.ForEach(func(key, value []byte) error {
			var candidate memberRecoveryLink
			if json.Unmarshal(value, &candidate) != nil {
				return nil
			}
			stored, decodeErr := hex.DecodeString(candidate.TokenDigest)
			if decodeErr == nil && len(stored) == len(digest) && subtle.ConstantTimeCompare(stored, digest[:]) == 1 {
				live = candidate
				matched = true
			}
			return nil
		}); err != nil {
			return err
		}
		applyNow := time.Now().UTC()
		if !matched {
			return errors.New("member link unavailable")
		}
		var txPrincipal Principal
		value := tx.Bucket([]byte(bucketPrincipals)).Get([]byte(live.PrincipalID))
		if value == nil || json.Unmarshal(value, &txPrincipal) != nil || !memberLinkUsable(live, &txPrincipal, applyNow) {
			return errors.New("member link unavailable")
		}
		updated = txPrincipal
		updated.PasswordHash = passwordHash
		if err := putJSON(tx.Bucket([]byte(bucketPrincipals)), updated.ID, &updated); err != nil {
			return err
		}
		if err := deletePrincipalSessions(tx, updated.ID); err != nil {
			return err
		}
		// Consuming a link retires every other pending link of this principal.
		return deleteMemberLinksForPrincipal(bucket, updated.ID)
	})
	if err != nil {
		return nil, "", "", err
	}
	_ = p.recordAudit(updated.ID, "member.password_set", updated.ID, link.Purpose)
	p.mu.Lock()
	p.principals[updated.ID] = &updated
	p.mu.Unlock()
	sessionToken, csrf, err := p.createSession(updated.ID)
	if err != nil {
		return nil, "", "", err
	}
	return copyPrincipal(&updated), sessionToken, csrf, nil
}

func deleteMemberLinksForPrincipal(bucket *bbolt.Bucket, principalID string) error {
	var keys [][]byte
	if err := bucket.ForEach(func(key, value []byte) error {
		var candidate memberRecoveryLink
		if json.Unmarshal(value, &candidate) == nil && candidate.PrincipalID == principalID {
			keys = append(keys, append([]byte(nil), key...))
		}
		return nil
	}); err != nil {
		return err
	}
	for _, key := range keys {
		if err := bucket.Delete(key); err != nil {
			return err
		}
	}
	return nil
}

func (p *PassportStore) hasOperator() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, principal := range p.principals {
		if principalIsUsableOperator(principal) {
			return true
		}
	}
	return false
}

func principalIsUsableOperator(principal *Principal) bool {
	return principal != nil && principal.Kind == PrincipalOperator && principal.Status == PrincipalActive && principal.ExpiresAt == nil
}

func (p *PassportStore) bootstrapOperator(username, email, displayName, password string) (*Principal, error) {
	if p.hasOperator() {
		return nil, errors.New("operator already exists")
	}
	if username == "" {
		username = "operator"
	}
	username, err := normalizeUsername(username)
	if err != nil {
		return nil, err
	}
	if p.byLogin(username) != nil {
		return nil, errors.New("username is already taken")
	}
	if len(password) < 12 {
		return nil, errors.New("password must be at least 12 characters")
	}
	passwordHash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	id, err := secureID(12)
	if err != nil {
		return nil, err
	}
	principal := &Principal{ID: id, Status: PrincipalActive, CreatedAt: time.Now().UTC()}
	updated := *principal
	updated.Kind = PrincipalOperator
	updated.Status = PrincipalActive
	updated.Note = "operator"
	updated.Username = username
	updated.Email = strings.ToLower(strings.TrimSpace(email))
	updated.DisplayName = strings.TrimSpace(displayName)
	updated.PasswordHash = passwordHash
	if updated.DisplayName == "" {
		updated.DisplayName = username
	}
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		if err := putJSON(tx.Bucket([]byte(bucketPrincipals)), updated.ID, &updated); err != nil {
			return err
		}
		return p.audit(tx, updated.ID, "operator.bootstrapped", updated.ID, username)
	}); err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.principals[updated.ID] = &updated
	p.mu.Unlock()
	return copyPrincipal(&updated), nil
}

func memberLinkURL(h *proxyHandler, r *http.Request, token string) string {
	return strings.TrimRight(h.getEffectivePublicURL(r), "/") + "/recover#" + token
}

func (h *proxyHandler) handleConsoleMembers(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	operator, session, ok := h.requireOperator(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost || !h.passportCSRF(r, session) {
		respondJSONError(w, http.StatusForbidden, "csrf validation failed")
		return
	}
	var input struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		Purpose     string `json:"purpose"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&input) != nil {
		respondJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if input.Purpose == "" {
		input.Purpose = "onboard"
	}
	result, err := h.passport.createMemberLink(operator.ID, input.Email, input.DisplayName, input.Purpose)
	if err != nil {
		respondJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, map[string]any{
		"principal":  publicPrincipal(result.Principal),
		"link":       memberLinkURL(h, r, result.Token),
		"expires_at": result.ExpiresAt,
	})
}

func (h *proxyHandler) handleMemberRecoveryStatus(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		Token string `json:"token"`
	}
	// Malformed bodies collapse into the same invalid answer as unknown,
	// expired, consumed, or replaced tokens: no informational difference.
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&input) != nil {
		respondJSON(w, map[string]any{"valid": false})
		return
	}
	h.passport.cleanupExpiredMemberLinks(time.Now().UTC())
	link, ok := h.passport.memberLinkStatus(input.Token)
	if !ok {
		respondJSON(w, map[string]any{"valid": false})
		return
	}
	now := time.Now().UTC()
	remaining := link.ExpiresAt.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	respondJSON(w, map[string]any{
		"valid":              true,
		"expires_at":         link.ExpiresAt,
		"expires_in_seconds": int64(remaining.Seconds()),
	})
}

func (h *proxyHandler) handleMemberRecovery(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := getClientIP(r)
	if h.bruteForce != nil && h.bruteForce.isBanned(ip) {
		respondJSONError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	var input struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&input) != nil {
		respondJSONError(w, http.StatusBadRequest, "This recovery link is unavailable.")
		return
	}
	principal, sessionToken, csrf, err := h.passport.redeemMemberLink(input.Token, input.Password)
	if err != nil {
		if err.Error() == "password verification busy" {
			respondJSONError(w, http.StatusServiceUnavailable, "password verification busy")
			return
		}
		if strings.Contains(err.Error(), "at least 12") {
			respondJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if h.bruteForce != nil {
			h.bruteForce.recordFailure(ip)
		}
		respondJSONError(w, http.StatusBadRequest, "This recovery link is unavailable.")
		return
	}
	if h.bruteForce != nil {
		h.bruteForce.recordSuccess(ip)
	}
	setSessionCookies(w, sessionToken, csrf)
	respondJSON(w, map[string]any{"principal": publicPrincipal(principal)})
}

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

const (
	bucketMemberRecoveryLinks        = "member_recovery_links"
	bucketMemberRecoveryLinksByToken = "member_recovery_links_by_token"
)

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
		linksByToken := tx.Bucket([]byte(bucketMemberRecoveryLinksByToken))
		// Operator-gated maintenance point for expired links.
		if err := cleanupExpiredMemberLinksTx(tx, now); err != nil {
			return err
		}
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
		if err := linksByToken.Put([]byte(link.TokenDigest), []byte(link.ID)); err != nil {
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

// memberLinkByDigestInTx resolves a recovery link through the digest index in
// O(1) instead of scanning the bucket, and constant-time confirms the stored
// digest still matches the presented one.
func memberLinkByDigestInTx(tx *bbolt.Tx, digestHex string) (memberRecoveryLink, bool) {
	var link memberRecoveryLink
	links := tx.Bucket([]byte(bucketMemberRecoveryLinks))
	index := tx.Bucket([]byte(bucketMemberRecoveryLinksByToken))
	if links == nil || index == nil {
		return link, false
	}
	id := index.Get([]byte(digestHex))
	if id == nil {
		return link, false
	}
	if json.Unmarshal(links.Get(id), &link) != nil {
		return link, false
	}
	stored, decodeErr := hex.DecodeString(link.TokenDigest)
	presented, presentedErr := hex.DecodeString(digestHex)
	if decodeErr != nil || presentedErr != nil || len(stored) != len(presented) || subtle.ConstantTimeCompare(stored, presented) != 1 {
		return memberRecoveryLink{}, false
	}
	return link, true
}

func (p *PassportStore) memberLinkByDigest(digest [32]byte) (memberRecoveryLink, bool) {
	var link memberRecoveryLink
	found := false
	_ = p.db.View(func(tx *bbolt.Tx) error {
		link, found = memberLinkByDigestInTx(tx, hex.EncodeToString(digest[:]))
		return nil
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
// active, unexpired member. Defense in depth: a link minted before the last
// password change stays dead even if its physical record survived a bug.
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
	if !principal.PasswordChangedAt.IsZero() && principal.PasswordChangedAt.After(link.CreatedAt) {
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

// cleanupExpiredMemberLinksTx lazily reclaims dead links and their index
// entries. Keys are collected as copies and deleted after the iteration,
// never inside ForEach. Expiry validation stays mandatory regardless of
// cleanup; this only runs inside operator- or token-gated transactions.
func cleanupExpiredMemberLinksTx(tx *bbolt.Tx, now time.Time) error {
	links := tx.Bucket([]byte(bucketMemberRecoveryLinks))
	index := tx.Bucket([]byte(bucketMemberRecoveryLinksByToken))
	type staleLink struct {
		key    []byte
		digest []byte
	}
	var expired []staleLink
	if err := links.ForEach(func(key, value []byte) error {
		var candidate memberRecoveryLink
		if json.Unmarshal(value, &candidate) == nil && memberLinkExpired(candidate, now) {
			expired = append(expired, staleLink{key: append([]byte(nil), key...), digest: []byte(candidate.TokenDigest)})
		}
		return nil
	}); err != nil {
		return err
	}
	for _, item := range expired {
		if err := links.Delete(item.key); err != nil {
			return err
		}
		if err := index.Delete(item.digest); err != nil {
			return err
		}
	}
	return nil
}

// setMemberPasswordLocked is the only in-transaction way a member password is
// set: hash rotation, PasswordChangedAt stamp, outstanding recovery-link
// invalidation, session invalidation, and audit all commit atomically.
func (p *PassportStore) setMemberPasswordLocked(tx *bbolt.Tx, principal *Principal, passwordHash, actorID, action, detail string, now time.Time) error {
	principal.PasswordHash = passwordHash
	principal.PasswordChangedAt = now
	if err := putJSON(tx.Bucket([]byte(bucketPrincipals)), principal.ID, principal); err != nil {
		return err
	}
	if err := deletePrincipalSessions(tx, principal.ID); err != nil {
		return err
	}
	if err := deleteMemberLinksForPrincipal(tx.Bucket([]byte(bucketMemberRecoveryLinks)), principal.ID); err != nil {
		return err
	}
	return p.audit(tx, actorID, action, principal.ID, detail)
}

// ChangePrincipalPassword is the centralized password-change operation. Every
// path that rotates a member password must go through it so recovery links and
// sessions are always retired together with the hash.
func (p *PassportStore) ChangePrincipalPassword(actorID, principalID, password string) (*Principal, error) {
	if len(password) < 12 {
		return nil, errors.New("password must be at least 12 characters")
	}
	principal := p.principal(principalID)
	if principal == nil || principal.Kind != PrincipalMember || principal.Status != PrincipalActive ||
		(principal.ExpiresAt != nil && !time.Now().UTC().Before(*principal.ExpiresAt)) {
		return nil, errors.New("member not found")
	}
	select {
	case p.passwordWork <- struct{}{}:
		defer func() { <-p.passwordWork }()
	default:
		return nil, errors.New("password verification busy")
	}
	passwordHash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	var updated Principal
	err = p.db.Update(func(tx *bbolt.Tx) error {
		value := tx.Bucket([]byte(bucketPrincipals)).Get([]byte(principalID))
		if value == nil || json.Unmarshal(value, &updated) != nil || updated.Kind != PrincipalMember || updated.Status != PrincipalActive {
			return errors.New("member not found")
		}
		return p.setMemberPasswordLocked(tx, &updated, passwordHash, actorID, "member.password_changed", "", time.Now().UTC())
	})
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.principals[updated.ID] = &updated
	p.mu.Unlock()
	return copyPrincipal(&updated), nil
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
		// Re-check the link inside the transaction through the digest index:
		// it may have been consumed between the pre-check and this write.
		live, matched := memberLinkByDigestInTx(tx, hex.EncodeToString(digest[:]))
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
		// Single redemption: consuming the link, rotating the hash, killing
		// sessions, retiring every pending link, and the audit all commit (or
		// roll back) as one transaction.
		if err := p.setMemberPasswordLocked(tx, &updated, passwordHash, txPrincipal.ID, "member.password_set", live.Purpose, applyNow); err != nil {
			return err
		}
		// Token-gated maintenance point for expired links.
		return cleanupExpiredMemberLinksTx(tx, applyNow)
	})
	if err != nil {
		return nil, "", "", err
	}
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
	var digests [][]byte
	if err := bucket.ForEach(func(key, value []byte) error {
		var candidate memberRecoveryLink
		if json.Unmarshal(value, &candidate) == nil && candidate.PrincipalID == principalID {
			keys = append(keys, append([]byte(nil), key...))
			digests = append(digests, []byte(candidate.TokenDigest))
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
	index := bucket.Tx().Bucket([]byte(bucketMemberRecoveryLinksByToken))
	if index == nil {
		return nil
	}
	for _, digest := range digests {
		if err := index.Delete(digest); err != nil {
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
	updated.PasswordChangedAt = time.Now().UTC()
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
	// Rate limit: banned IPs are refused, but status probes never count as
	// failures themselves (a stale link must not lock its owner out).
	if ip := getClientIP(r); h.bruteForce != nil && h.bruteForce.isBanned(ip) {
		respondJSONError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
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
	// Read-only path: hash, indexed lookup, validity, answer. No write
	// transactions and no bucket scans are reachable by anonymous callers.
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

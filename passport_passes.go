package main

import (
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

const (
	bucketJoinLinks           = "join_links"
	bucketJoinLinksByToken    = "join_links_by_token"
	bucketPassportAudit       = "passport_audit"
	bucketWebAuthnCredentials = "webauthn_credentials"
	bucketWebAuthnChallenges  = "webauthn_challenges"
)

type JoinLink struct {
	ID              string     `json:"id"`
	PrincipalID     string     `json:"principal_id"`
	CreatedBy       string     `json:"created_by"`
	TokenDigest     string     `json:"token_digest"`
	TokenCiphertext []byte     `json:"token_ciphertext"`
	CreatedAt       time.Time  `json:"created_at"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	LastUsedAt      *time.Time `json:"last_used_at,omitempty"`
	Revoked         bool       `json:"revoked"`
}
type AuditEntry struct {
	ID        string    `json:"id"`
	ActorID   string    `json:"actor_id"`
	Action    string    `json:"action"`
	SubjectID string    `json:"subject_id"`
	At        time.Time `json:"at"`
	Detail    string    `json:"detail,omitempty"`
}

type PassView struct {
	ID          string          `json:"id"`
	Note        string          `json:"note"`
	DisplayName string          `json:"display_name,omitempty"`
	AvatarURL   string          `json:"avatar_url,omitempty"`
	Status      PrincipalStatus `json:"status"`
	ExpiresAt   *time.Time      `json:"expires_at,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	CreatedBy   string          `json:"created_by"`
	Link        string          `json:"link"`
	Clients     int             `json:"clients"`
}

func putJSON(bucket *bbolt.Bucket, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return bucket.Put([]byte(key), data)
}
func (p *PassportStore) audit(tx *bbolt.Tx, actor, action, subject, detail string) error {
	id, err := secureID(9)
	if err != nil {
		return err
	}
	e := AuditEntry{ID: id, ActorID: actor, Action: action, SubjectID: subject, At: time.Now().UTC(), Detail: detail}
	bucket := tx.Bucket([]byte(bucketPassportAudit))
	if bucket == nil { return errors.New("audit storage unavailable") }
	return putJSON(bucket, e.At.Format(time.RFC3339Nano)+"|"+id, e)
}

func (p *PassportStore) recordAudit(actor, action, subject, detail string) error {
	if p == nil {
		return nil
	}
	return p.db.Update(func(tx *bbolt.Tx) error {
		return p.audit(tx, actor, action, subject, detail)
	})
}

func (h *proxyHandler) auditProviderContribution(r *http.Request, provider, accountID string) {
	if h.passport == nil {
		return
	}
	if err := h.passport.recordAudit(providerContributionActor(r), "provider.account_added", accountID, provider); err != nil {
		log.Printf("record provider contribution audit: %v", err)
	}
}

func (p *PassportStore) createGuest(actorID, note, displayName string, expires *time.Time) (*Principal, *JoinLink, *ClientCredential, string, error) {
	note = strings.TrimSpace(note)
	if note == "" || len([]rune(note)) > 300 {
		return nil, nil, nil, "", errors.New("note required (max 300 characters)")
	}
	id, err := secureID(12)
	if err != nil {
		return nil, nil, nil, "", err
	}
	linkID, err := secureID(9)
	if err != nil {
		return nil, nil, nil, "", err
	}
	token, err := secureToken(32)
	if err != nil {
		return nil, nil, nil, "", err
	}
	clientID, err := secureID(9)
	if err != nil {
		return nil, nil, nil, "", err
	}
	download, err := secureToken(24)
	if err != nil {
		return nil, nil, nil, "", err
	}
	now := time.Now().UTC()
	pr := &Principal{ID: id, Kind: PrincipalGuest, Status: PrincipalActive, Note: note, DisplayName: strings.TrimSpace(displayName), ExpiresAt: expires, CreatedBy: actorID, CreatedAt: now}
	linkDigest := hashToken(token)
	link := &JoinLink{ID: linkID, PrincipalID: id, CreatedBy: actorID, TokenDigest: hex.EncodeToString(linkDigest[:]), CreatedAt: now, ExpiresAt: expires}
	link.TokenCiphertext, err = p.seal("join", link.ID, id, token)
	if err != nil {
		return nil, nil, nil, "", err
	}
	client := &ClientCredential{ID: clientID, PrincipalID: id, Label: "DEFAULT", Status: "active", ExpiresAt: expires, DownloadToken: download, CreatedAt: now}
	dd := hashToken(download)
	client.DownloadDigest = hex.EncodeToString(dd[:])
	client.DownloadCiphertext, err = p.seal("client", client.ID, id, download)
	if err != nil {
		return nil, nil, nil, "", err
	}
	err = p.db.Update(func(tx *bbolt.Tx) error {
		if err := putJSON(tx.Bucket([]byte(bucketPrincipals)), id, pr); err != nil {
			return err
		}
		if err := putJSON(tx.Bucket([]byte(bucketJoinLinks)), linkID, link); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(bucketJoinLinksByToken)).Put([]byte(link.TokenDigest), []byte(linkID)); err != nil {
			return err
		}
		if err := putJSON(tx.Bucket([]byte(bucketClientCredentials)), clientID, client); err != nil {
			return err
		}
		return p.audit(tx, actorID, "guest.created", id, note)
	})
	if err != nil {
		return nil, nil, nil, "", err
	}
	p.mu.Lock()
	p.principals[id] = pr
	p.clients[clientID] = client
	p.mu.Unlock()
	return pr, link, client, token, nil
}
func (p *PassportStore) linkToken(link *JoinLink) (string, error) {
	return p.open("join", link.ID, link.PrincipalID, link.TokenCiphertext)
}
func (p *PassportStore) linkByToken(token string) (*JoinLink, error) {
	digest := hashToken(token)
	want := hex.EncodeToString(digest[:])
	var found *JoinLink
	err := p.db.View(func(tx *bbolt.Tx) error {
		indexBucket := tx.Bucket([]byte(bucketJoinLinksByToken))
		if indexBucket == nil {
			return errors.New("join links index unavailable")
		}
		linkID := indexBucket.Get([]byte(want))
		if linkID == nil {
			return nil
		}
		raw := tx.Bucket([]byte(bucketJoinLinks)).Get(linkID)
		if raw == nil {
			return nil
		}
		var x JoinLink
		if json.Unmarshal(raw, &x) == nil && subtle.ConstantTimeCompare([]byte(x.TokenDigest), []byte(want)) == 1 {
			found = &x
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, errors.New("invalid link")
	}
	return found, nil
}
func (p *PassportStore) redeemJoin(token string) (*Principal, string, string, error) {
	link, err := p.linkByToken(token)
	if err != nil || link.Revoked || (link.ExpiresAt != nil && time.Now().After(*link.ExpiresAt)) {
		return nil, "", "", errors.New("link unavailable")
	}
	pr := p.principal(link.PrincipalID)
	if pr == nil || pr.Status != PrincipalActive || (pr.ExpiresAt != nil && time.Now().After(*pr.ExpiresAt)) {
		return nil, "", "", errors.New("link unavailable")
	}
	now := time.Now().UTC()
	link.LastUsedAt = &now
	if err = p.db.Update(func(tx *bbolt.Tx) error { return putJSON(tx.Bucket([]byte(bucketJoinLinks)), link.ID, link) }); err != nil {
		return nil, "", "", err
	}
	session, csrf, err := p.createSession(pr.ID)
	return pr, session, csrf, err
}
func (p *PassportStore) listPasses(actor *Principal) ([]PassView, error) {
	var links []JoinLink
	if err := p.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(bucketJoinLinks)).ForEach(func(_, v []byte) error {
			var x JoinLink
			if err := json.Unmarshal(v, &x); err == nil {
				links = append(links, x)
			}
			return nil
		})
	}); err != nil {
		return nil, err
	}
	out := make([]PassView, 0, len(links))
	for _, l := range links {
		pr := p.principal(l.PrincipalID)
		if pr == nil || pr.Kind != PrincipalGuest {
			continue
		}
		if actor.Kind != PrincipalOperator && pr.CreatedBy != actor.ID {
			continue
		}
		token, _ := p.linkToken(&l)
		clients := 0
		p.mu.RLock()
		for _, c := range p.clients {
			if c.PrincipalID == pr.ID {
				clients++
			}
		}
		p.mu.RUnlock()
		avatar := ""
		if pr.AvatarUpdatedAt != nil {
			avatar = "/api/avatars/" + pr.ID + "?v=" + pr.AvatarUpdatedAt.Format("20060102T150405")
		}
		out = append(out, PassView{ID: pr.ID, Note: pr.Note, DisplayName: pr.DisplayName, AvatarURL: avatar, Status: pr.Status, ExpiresAt: pr.ExpiresAt, CreatedAt: pr.CreatedAt, CreatedBy: pr.CreatedBy, Link: "/join#" + token, Clients: clients})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (h *proxyHandler) requireMember(w http.ResponseWriter, r *http.Request) (*Principal, *passportSession, bool) {
	pr, s := h.passport.authenticate(r)
	if pr == nil || (pr.Kind != PrincipalMember && pr.Kind != PrincipalOperator) {
		respondJSONError(w, 403, "member access required")
		return nil, nil, false
	}
	return pr, s, true
}
func (h *proxyHandler) handlePasses(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	actor, s, ok := h.requireMember(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		passes, err := h.passport.listPasses(actor)
		if err != nil {
			respondJSONError(w, 500, "passes unavailable")
			return
		}
		respondJSON(w, passes)
	case http.MethodPost:
		if !h.passportCSRF(r, s) {
			respondJSONError(w, 403, "csrf validation failed")
			return
		}
		var q struct {
			Note        string     `json:"note"`
			DisplayName string     `json:"display_name"`
			ExpiresAt   *time.Time `json:"expires_at"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&q) != nil {
			respondJSONError(w, 400, "invalid json")
			return
		}
		pr, _, _, token, err := h.passport.createGuest(actor.ID, q.Note, q.DisplayName, q.ExpiresAt)
		if err != nil {
			respondJSONError(w, 400, err.Error())
			return
		}
		respondJSON(w, map[string]any{"principal": publicPrincipal(pr), "link": "/join#" + token})
	default:
		http.Error(w, "method not allowed", 405)
	}
}
func (h *proxyHandler) handleJoin(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	ip := getClientIP(r)
	if h.bruteForce != nil && h.bruteForce.isBanned(ip) {
		respondJSONError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	var q struct {
		Token  string `json:"token"`
		Switch bool   `json:"switch"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&q) != nil {
		respondJSONError(w, 400, "invalid json")
		return
	}
	if current, _ := h.passport.authenticate(r); current != nil && !q.Switch {
		link, err := h.passport.linkByToken(q.Token)
		if err == nil && !link.Revoked && (link.ExpiresAt == nil || time.Now().Before(*link.ExpiresAt)) {
			pr := h.passport.principal(link.PrincipalID)
			if pr != nil && pr.Status == PrincipalActive && (pr.ExpiresAt == nil || time.Now().Before(*pr.ExpiresAt)) {
				if link.PrincipalID != current.ID {
					respondJSON(w, map[string]any{"switch_required": true, "current": publicPrincipal(current)})
					return
				}
			}
		}
	}
	pr, session, csrf, err := h.passport.redeemJoin(q.Token)
	if err != nil {
		if h.bruteForce != nil {
			h.bruteForce.recordFailure(ip)
		}
		h.metrics.incPassport("join_redemptions", "failed")
		respondJSONError(w, 404, "this pass is unavailable")
		return
	}
	if h.bruteForce != nil {
		h.bruteForce.recordSuccess(ip)
	}
	h.metrics.incPassport("join_redemptions", "succeeded")
	setSessionCookies(w, session, csrf)
	respondJSON(w, map[string]any{"principal": publicPrincipal(pr), "csrf": csrf})
}

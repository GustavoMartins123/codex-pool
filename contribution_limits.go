package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.etcd.io/bbolt"
)

const (
	bucketContributionAttempts       = "account_contribution_attempts"
	contributionAttemptsPerHour      = 20
	contributionAccountsPerPrincipal = 32
	contributionPendingTTL           = 10 * time.Minute
)

type contributionAttemptWindow struct {
	StartedAt time.Time `json:"started_at"`
	Attempts  int       `json:"attempts"`
}

func (p *PassportStore) admitContributionAttempt(actor string, now time.Time) error {
	if !p.contributionActorAllowed(actor) {
		return errors.New("account contribution is not authorized")
	}
	return p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketContributionAttempts))
		if b == nil {
			return errors.New("account contribution limits unavailable")
		}
		var window contributionAttemptWindow
		if raw := b.Get([]byte(actor)); raw != nil {
			if err := json.Unmarshal(raw, &window); err != nil {
				return err
			}
			if window.StartedAt.IsZero() || window.Attempts < 0 {
				return errors.New("invalid account contribution limits")
			}
		}
		if window.StartedAt.IsZero() || !now.Before(window.StartedAt.Add(time.Hour)) {
			window = contributionAttemptWindow{StartedAt: now.UTC()}
		}
		if window.Attempts >= contributionAttemptsPerHour {
			return &policyError{Status: 429, Code: "contribution_rate_limited", Message: "account registration attempt limit reached; retry after the current hour window"}
		}
		window.Attempts++
		return putJSON(b, actor, window)
	})
}

func (h *proxyHandler) checkContributionAttempt(w http.ResponseWriter, r *http.Request) bool {
	if h.passport == nil {
		respondJSONError(w, 503, "account authority unavailable")
		return false
	}
	if err := h.passport.admitContributionAttempt(providerContributionActor(r), time.Now()); err != nil {
		respondPolicyError(w, err)
		return false
	}
	return true
}

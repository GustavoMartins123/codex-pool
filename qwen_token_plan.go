package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	qwenCreditPollInterval = 5 * time.Minute
	qwenPersonalManual     = "personal_manual"
	qwenTeamAPI            = "team_api"
	qwenPersonalCloud      = "personal_cloud"
	defaultQwenPlanBase    = "https://modelstudio.ap-southeast-1.aliyuncs.com"
)

type QwenTokenPlanConfig struct {
	Mode            string    `json:"mode"`
	Plan            string    `json:"plan,omitempty"`
	TotalCredits    float64   `json:"total_credits,omitempty"`
	Remaining       float64   `json:"remaining_credits,omitempty"`
	CycleStart      time.Time `json:"cycle_start,omitempty"`
	CycleEnd        time.Time `json:"cycle_end,omitempty"`
	AccessKeyID     string    `json:"access_key_id,omitempty"`
	AccessKeySecret string    `json:"access_key_secret,omitempty"`
	SecurityToken   string    `json:"security_token,omitempty"`
	SeatID          string    `json:"seat_id,omitempty"`
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
}

type QwenCreditSnapshot struct {
	Edition    string    `json:"edition"`
	Plan       string    `json:"plan"`
	Total      float64   `json:"total"`
	Used       float64   `json:"used"`
	Remaining  float64   `json:"remaining"`
	CycleStart time.Time `json:"cycle_start"`
	CycleEnd   time.Time `json:"cycle_end"`
	UpdatedAt  time.Time `json:"updated_at"`
	Source     string    `json:"source"`
}

func validateQwenTokenPlan(plan *QwenTokenPlanConfig, now time.Time) error {
	if plan == nil {
		return nil
	}
	switch plan.Mode {
	case qwenPersonalManual:
		if plan.AccessKeyID != "" || plan.AccessKeySecret != "" || plan.SecurityToken != "" || plan.SeatID != "" {
			return errors.New("Personal manual tracking does not accept Team credentials")
		}
		if !validQwenCreditAmounts(plan.TotalCredits, plan.Remaining) {
			return errors.New("credit total must be positive and remaining credits must be between zero and total")
		}
		if plan.CycleStart.IsZero() || plan.CycleStart.After(now) || !plan.CycleEnd.After(now) || !plan.CycleEnd.After(plan.CycleStart) {
			return errors.New("the subscription cycle must contain the current time")
		}
	case qwenTeamAPI:
		if strings.TrimSpace(plan.AccessKeyID) == "" || strings.TrimSpace(plan.AccessKeySecret) == "" || strings.TrimSpace(plan.SeatID) == "" {
			return errors.New("Team credit tracking requires AccessKey ID, AccessKey secret and seat ID")
		}
		if plan.TotalCredits != 0 || plan.Remaining != 0 || !plan.CycleStart.IsZero() || !plan.CycleEnd.IsZero() {
			return errors.New("Team credits and cycle dates must come from Alibaba Cloud")
		}
	case qwenPersonalCloud:
		if strings.TrimSpace(plan.AccessKeyID) == "" || strings.TrimSpace(plan.AccessKeySecret) == "" {
			return errors.New("Alibaba Cloud tracking requires an AccessKey ID and AccessKey secret")
		}
		if plan.SeatID != "" || plan.TotalCredits != 0 || plan.Remaining != 0 || !plan.CycleStart.IsZero() || !plan.CycleEnd.IsZero() {
			return errors.New("Cloud credits and cycle dates must come from Alibaba Cloud")
		}
	default:
		return errors.New("unsupported Token Plan credit source")
	}
	return nil
}

func validQwenCreditAmounts(total, remaining float64) bool {
	return !math.IsNaN(total) && !math.IsInf(total, 0) && !math.IsNaN(remaining) && !math.IsInf(remaining, 0) && total > 0 && remaining >= 0 && remaining <= total
}

func qwenCreditUsage(credits *QwenCreditSnapshot, now time.Time) UsageSnapshot {
	used := credits.Used / credits.Total
	if credits.Source != "manual" {
		now = credits.UpdatedAt
	}
	return UsageSnapshot{
		PrimaryUsed: used, PrimaryUsedPercent: used, PrimaryUsageReported: true,
		PrimaryWindowMinutes: int(credits.CycleEnd.Sub(credits.CycleStart).Minutes()),
		PrimaryResetAt:       credits.CycleEnd, CreditsBalance: credits.Remaining,
		HasCredits: credits.Remaining > 0, RetrievedAt: now, Source: "qwen-token-plan",
		primarySet: true, secondarySet: true, creditsSet: true,
	}
}

func signQwenPlanRequest(req *http.Request, plan *QwenTokenPlanConfig, action string, now time.Time, nonce string) {
	const algorithm = "ACS3-HMAC-SHA256"
	emptyHash := sha256.Sum256(nil)
	req.Header.Set("x-acs-action", action)
	req.Header.Set("x-acs-version", "2026-02-10")
	req.Header.Set("x-acs-date", now.UTC().Format("2006-01-02T15:04:05Z"))
	req.Header.Set("x-acs-content-sha256", hex.EncodeToString(emptyHash[:]))
	req.Header.Set("x-acs-signature-nonce", nonce)
	keys := []string{"host", "x-acs-action", "x-acs-content-sha256", "x-acs-date", "x-acs-signature-nonce", "x-acs-version"}
	if plan.SecurityToken != "" {
		req.Header.Set("x-acs-security-token", plan.SecurityToken)
		keys = append(keys, "x-acs-security-token")
	}
	sort.Strings(keys)
	var canonicalHeaders strings.Builder
	for _, key := range keys {
		value := req.Header.Get(key)
		if key == "host" {
			value = req.URL.Host
		}
		canonicalHeaders.WriteString(key + ":" + strings.TrimSpace(value) + "\n")
	}
	signedHeaders := strings.Join(keys, ";")
	query := strings.ReplaceAll(req.URL.Query().Encode(), "+", "%20")
	canonical := strings.Join([]string{req.Method, req.URL.EscapedPath(), query, canonicalHeaders.String(), signedHeaders, hex.EncodeToString(emptyHash[:])}, "\n")
	digest := sha256.Sum256([]byte(canonical))
	mac := hmac.New(sha256.New, []byte(plan.AccessKeySecret))
	mac.Write([]byte(algorithm + "\n" + hex.EncodeToString(digest[:])))
	req.Header.Set("Authorization", algorithm+" Credential="+plan.AccessKeyID+",SignedHeaders="+signedHeaders+",Signature="+hex.EncodeToString(mac.Sum(nil)))
}

func (h *proxyHandler) queryQwenCredits(ctx context.Context, plan *QwenTokenPlanConfig, now time.Time) (*QwenCreditSnapshot, error) {
	if plan == nil {
		return nil, nil
	}
	if err := validateQwenTokenPlan(plan, now); err != nil {
		return nil, err
	}
	if plan.Mode == qwenPersonalManual {
		updated := plan.UpdatedAt
		if updated.IsZero() {
			updated = now.UTC()
		}
		return &QwenCreditSnapshot{Edition: "personal", Plan: plan.Plan,
			Total: plan.TotalCredits, Remaining: plan.Remaining, Used: plan.TotalCredits - plan.Remaining,
			CycleStart: plan.CycleStart, CycleEnd: plan.CycleEnd, UpdatedAt: updated, Source: "manual",
		}, nil
	}
	if plan.Mode == qwenPersonalCloud {
		return h.queryQwenCloudPersonalCredits(ctx, plan, now)
	}
	if h.cfg.qwenPlanBase == nil {
		return nil, errors.New("Qwen Token Plan management endpoint is not configured")
	}
	target := *h.cfg.qwenPlanBase
	target.Path = singleJoin(target.Path, "/tokenplan/subscription/seat-detail")
	query := url.Values{"PageNo": {"1"}, "PageSize": {"10"}, "SeatId": {plan.SeatID}, "QueryAssigned": {"true"}, "StatusList.1": {"NORMAL"}}
	target.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, errors.New("could not create Token Plan credit request")
	}
	signQwenPlanRequest(req, plan, "GetSubscriptionSeatDetails", now, uuid.NewString())
	req.Header.Set("Accept", "application/json")
	resp, err := h.transport.RoundTrip(req)
	if err != nil {
		return nil, errors.New("Token Plan credit request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Token Plan credit request returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, errors.New("could not read Token Plan credit response")
	}
	return parseQwenTeamCredits(body, plan.SeatID, now)
}

func parseQwenTeamCredits(body []byte, seatID string, now time.Time) (*QwenCreditSnapshot, error) {
	var payload struct {
		Success bool
		Data    struct {
			Items []struct {
				SeatID     string `json:"SeatId"`
				SpecType   string
				Status     string
				EquityList []struct {
					EquityType        string
					CycleStartTime    int64
					CycleEndTime      int64
					CycleTotalValue   *float64
					CycleSurplusValue *float64
				}
			}
		}
	}
	if json.Unmarshal(body, &payload) != nil || !payload.Success {
		return nil, errors.New("Alibaba Cloud did not return valid Token Plan credit data")
	}
	var credits *QwenCreditSnapshot
	for _, seat := range payload.Data.Items {
		if seat.SeatID != seatID || seat.Status != "NORMAL" {
			continue
		}
		for _, equity := range seat.EquityList {
			if equity.EquityType != "CREDITS" {
				continue
			}
			start, end := time.UnixMilli(equity.CycleStartTime), time.UnixMilli(equity.CycleEndTime)
			if equity.CycleTotalValue == nil || equity.CycleSurplusValue == nil || !validQwenCreditAmounts(*equity.CycleTotalValue, *equity.CycleSurplusValue) || start.After(now) || !end.After(now) || !end.After(start) {
				return nil, errors.New("Token Plan returned invalid credit amounts or an inactive subscription cycle")
			}
			if credits != nil {
				return nil, errors.New("Token Plan returned ambiguous seat credit data")
			}
			credits = &QwenCreditSnapshot{Edition: "team", Plan: seat.SpecType,
				Total: *equity.CycleTotalValue, Remaining: *equity.CycleSurplusValue,
				Used:       *equity.CycleTotalValue - *equity.CycleSurplusValue,
				CycleStart: start.UTC(), CycleEnd: end.UTC(), UpdatedAt: now.UTC(), Source: "alibaba-api",
			}
		}
	}
	if credits == nil {
		return nil, errors.New("no active Token Plan credit entitlement was returned for this seat")
	}
	return credits, nil
}

func (h *proxyHandler) fetchQwenCreditUsage(ctx context.Context, now time.Time, account *Account) error {
	account.mu.Lock()
	plan := account.QwenTokenPlan
	account.mu.Unlock()
	credits, err := h.queryQwenCredits(ctx, plan, now)
	account.mu.Lock()
	defer account.mu.Unlock()
	if account.QwenTokenPlan != plan {
		return errors.New("Token Plan configuration changed during credit refresh")
	}
	account.QwenCredits = credits
	account.QwenCreditsError = ""
	if err != nil {
		account.QwenCreditsError = err.Error()
	} else if plan == nil {
		account.QwenCreditsError = "Credit tracking is not configured. Connect your Alibaba Cloud account or select Personal manual tracking."
	}
	if credits != nil {
		account.Usage = qwenCreditUsage(credits, now)
	} else {
		account.Usage = UsageSnapshot{Source: "qwen-token-plan", RetrievedAt: now, primarySet: true, secondarySet: true, creditsSet: true}
	}
	if saveErr := saveAccount(account); saveErr != nil {
		return errors.Join(err, saveErr)
	}
	return err
}

func qwenCreditsCurrent(credits *QwenCreditSnapshot, now time.Time) bool {
	return credits != nil && validQwenCreditAmounts(credits.Total, credits.Remaining) &&
		!credits.CycleStart.After(now) && credits.CycleEnd.After(now) &&
		!credits.UpdatedAt.After(now) && !credits.UpdatedAt.IsZero() &&
		(credits.Source == "manual" || now.Sub(credits.UpdatedAt) <= qwenCreditPollInterval)
}

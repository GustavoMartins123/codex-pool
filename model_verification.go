package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

const bucketModelVerifications = "model_verifications"
const modelVerificationSuite = "codex-pool/1"
const modelVerificationTTL = 7 * 24 * time.Hour

type modelVerificationCheck struct {
	Status string `json:"status"`
	Code   string `json:"code"`
}

type modelVerification struct {
	Suite      string                            `json:"suite"`
	Model      string                            `json:"model"`
	Provider   AccountType                       `json:"provider"`
	Runtime    string                            `json:"runtime"`
	Codex      string                            `json:"codex_version"`
	StartedAt  time.Time                         `json:"started_at"`
	FinishedAt time.Time                         `json:"finished_at"`
	Checks     map[string]modelVerificationCheck `json:"checks"`
	Expired    bool                              `json:"expired"`
}

var verificationVersionPattern = regexp.MustCompile(`^[a-zA-Z0-9 ._+()/-]{1,80}$`)
var verificationRuntimeOnce sync.Once
var verificationRuntime string
var verificationRuntimeErr error

func modelVerificationRuntime() (string, error) {
	verificationRuntimeOnce.Do(func() {
		var path string
		path, verificationRuntimeErr = os.Executable()
		if verificationRuntimeErr != nil {
			return
		}
		var file *os.File
		file, verificationRuntimeErr = os.Open(path)
		if verificationRuntimeErr != nil {
			return
		}
		defer file.Close()
		hash := sha256.New()
		if _, verificationRuntimeErr = io.Copy(hash, file); verificationRuntimeErr == nil {
			verificationRuntime = hex.EncodeToString(hash.Sum(nil))
		}
	})
	return verificationRuntime, verificationRuntimeErr
}

func (v modelVerification) validate() error {
	decoded, err := hex.DecodeString(v.Runtime)
	if v.Suite != modelVerificationSuite || v.Model == "" || len(v.Model) > 200 || v.Provider == "" ||
		err != nil || len(decoded) != sha256.Size || !verificationVersionPattern.MatchString(v.Codex) ||
		v.StartedAt.IsZero() || v.FinishedAt.Before(v.StartedAt) || v.FinishedAt.Sub(v.StartedAt) > 30*time.Minute || len(v.Checks) != 4 {
		return errors.New("invalid model verification")
	}
	for _, name := range []string{"text", "streaming", "tools", "codex_edit"} {
		check, ok := v.Checks[name]
		if !ok {
			return errors.New("incomplete model verification")
		}
		valid := check.Status == "passed" && check.Code == map[string]string{
			"text": "exact_response", "streaming": "completed_stream", "tools": "tool_round_trip", "codex_edit": "file_edit_verified",
		}[name] || check.Status == "failed" && (check.Code == "request_failed" || check.Code == "unexpected_response" || check.Code == "client_failed" || check.Code == "timeout") ||
			check.Status == "not_tested" && check.Code == "not_run"
		if !valid {
			return errors.New("invalid verification check")
		}
	}
	if v.Checks["codex_edit"].Status == "passed" && v.Codex == "unavailable" {
		return errors.New("Codex version required for an editing verification")
	}
	return nil
}

func readModelVerifications(tx *bbolt.Tx, now time.Time) (map[string]modelVerification, error) {
	runtime, err := modelVerificationRuntime()
	if err != nil {
		return nil, err
	}
	bucket := tx.Bucket([]byte(bucketModelVerifications))
	if bucket == nil {
		return nil, errors.New("model verifications unavailable")
	}
	results := map[string]modelVerification{}
	err = bucket.ForEach(func(key, raw []byte) error {
		var v modelVerification
		if json.Unmarshal(raw, &v) != nil || v.validate() != nil || string(key) != resourceKey(v.Provider, v.Model) {
			return errors.New("invalid stored model verification")
		}
		v.Expired = v.Runtime != runtime || v.FinishedAt.After(now) || now.Sub(v.FinishedAt) > modelVerificationTTL
		results[string(key)] = v
		return nil
	})
	return results, err
}

func (h *proxyHandler) handleVerificationTarget(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		respondJSONError(w, 405, "method not allowed")
		return
	}
	if !h.requirePoolCredential(w, r) {
		return
	}
	runtime, err := modelVerificationRuntime()
	if err != nil {
		respondJSONError(w, 503, "verification runtime unavailable")
		return
	}
	respondJSON(w, map[string]string{"suite": modelVerificationSuite, "runtime": runtime})
}

// Only a live operator session can attest results produced by the local runner.
// Pool API keys and member sessions cannot publish compatibility claims.
func (h *proxyHandler) handleModelVerification(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if h.passport == nil {
		respondJSONError(w, 503, "verification authority unavailable")
		return
	}
	principal, session, ok := h.requireOperator(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		respondJSONError(w, 405, "method not allowed")
		return
	}
	if !h.passportCSRF(r, session) {
		respondJSONError(w, 403, "csrf validation failed")
		return
	}
	var verification modelVerification
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&verification) != nil || decoder.Decode(new(any)) != io.EOF || verification.validate() != nil || verification.Expired {
		respondJSONError(w, 400, "invalid model verification")
		return
	}
	now := time.Now().UTC()
	runtime, err := modelVerificationRuntime()
	if err != nil {
		respondJSONError(w, 503, "verification runtime unavailable")
		return
	}
	if verification.Runtime != runtime || verification.FinishedAt.After(now) || now.Sub(verification.FinishedAt) > modelVerificationTTL {
		respondJSONError(w, 409, "verification does not match the current runtime or is expired")
		return
	}
	if h.pool == nil || h.cfg == nil || h.pool.accountAuthority != h.passport {
		respondJSONError(w, 503, "verification authority unavailable")
		return
	}
	snapshot, err := h.memberQuotaSnapshot(principal.ID, now)
	if err != nil {
		respondJSONError(w, 503, "verification authority unavailable")
		return
	}
	found := false
	for _, model := range poolModelDescriptors(snapshot) {
		if model.ID == verification.Model && model.Provider == string(verification.Provider) && model.Provider != "pool" && model.SupportingAccounts > 0 {
			found = true
			break
		}
	}
	if !found {
		respondJSONError(w, 400, "model is not available in the operator catalog")
		return
	}
	err = h.passport.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketModelVerifications))
		key := resourceKey(verification.Provider, verification.Model)
		if raw := bucket.Get([]byte(key)); raw != nil {
			var previous modelVerification
			if json.Unmarshal(raw, &previous) != nil || previous.validate() != nil {
				return errors.New("invalid stored model verification")
			}
			if !verification.FinishedAt.After(previous.FinishedAt) {
				return accountControlError("verification_outdated", 409)
			}
		}
		if err := putJSON(bucket, key, verification); err != nil {
			return err
		}
		return h.passport.audit(tx, principal.ID, "model.verified", verification.Model, string(verification.Provider))
	})
	if err != nil {
		var policy *policyError
		if errors.As(err, &policy) && policy.Status == 409 {
			respondJSONError(w, 409, "a newer verification is already registered")
		} else {
			respondJSONError(w, 503, "unable to store model verification")
		}
		return
	}
	respondJSON(w, verification)
}

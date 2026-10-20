package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func testModelVerification(t *testing.T) modelVerification {
	t.Helper()
	runtime, err := modelVerificationRuntime()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Second)
	return modelVerification{Suite: modelVerificationSuite, Model: "claude-opus-5-5", Provider: AccountTypeClaude, Runtime: runtime, Codex: "codex-cli 0.159.1", StartedAt: now.Add(-time.Minute), FinishedAt: now,
		Checks: map[string]modelVerificationCheck{"text": {"passed", "exact_response"}, "streaming": {"passed", "completed_stream"}, "tools": {"passed", "tool_round_trip"}, "codex_edit": {"passed", "file_edit_verified"}}}
}

func postModelVerification(t *testing.T, h *proxyHandler, session, csrf string, v any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, passportContributionRequest("POST", "/api/console/model-verifications", session, csrf, body))
	return w
}

func TestModelVerificationAuthorityAndProjection(t *testing.T) {
	h, id, member, csrf, operator, opCSRF := memberQuotaFixture(t)
	v := testModelVerification(t)
	for _, credentials := range [][2]string{{"", ""}, {member, csrf}, {operator, ""}} {
		if w := postModelVerification(t, h, credentials[0], credentials[1], v); w.Code != 403 {
			t.Fatalf("untrusted import: %d %s", w.Code, w.Body.String())
		}
	}
	if w := postModelVerification(t, h, operator, opCSRF, v); w.Code != 200 {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	if rows := quotaHTTP(t, h, member); len(rows) != 0 {
		t.Fatal("verification bypassed blocked member policy")
	}
	saveQuotaPolicy(t, h, id, ClientPolicy{})
	rows := quotaHTTP(t, h, member)
	for _, row := range rows {
		if row.Provider == "claude" {
			if row.Compatibility == nil || row.Compatibility.Expired || row.Compatibility.Checks["tools"].Status != "passed" {
				t.Fatalf("verification missing: %+v", row)
			}
		} else if row.Compatibility != nil {
			t.Fatal("verification leaked to a different model")
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, passportContributionRequest("GET", "/api/pool/catalog", member, "", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"tool_round_trip"`) {
		t.Fatalf("catalog verification missing: %d %s", w.Code, w.Body.String())
	}
	if w := postModelVerification(t, h, operator, opCSRF, v); w.Code != 409 {
		t.Fatal("older verification replaced latest result")
	}
	v.FinishedAt = time.Now().UTC()
	v.Checks["tools"] = modelVerificationCheck{"failed", "unexpected_response"}
	if w := postModelVerification(t, h, operator, opCSRF, v); w.Code != 200 {
		t.Fatalf("failure import: %d %s", w.Code, w.Body.String())
	}
	for _, row := range quotaHTTP(t, h, member) {
		if row.Provider == "claude" && row.Compatibility.Checks["tools"].Status != "failed" {
			t.Fatal("a failure retained the earlier success badge")
		}
	}
	if err := h.passport.revokeAccountGrant(memberQuotaOperator(t, h), "quota-claude", 1); err != nil {
		t.Fatal(err)
	}
	for _, row := range quotaHTTP(t, h, member) {
		if row.Provider == "claude" {
			t.Fatal("verification survived revoked account access")
		}
	}
}

func TestModelVerificationTargetAndBearerAuthority(t *testing.T) {
	h, _, _, _, _, _ := memberQuotaFixture(t)
	operator := memberQuotaOperator(t, h)
	client, err := h.passport.createClient(operator, "verifier", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("POOL_JWT_SECRET", "verification-test-secret")
	token := generateClaudePoolToken(getPoolJWTSecret(), operator+"-c-"+client.ID)
	request := func(method, path, bearer string, payload any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(payload)
		r := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request(http.MethodGet, "/api/verification/target", "", nil); w.Code != 401 {
		t.Fatalf("anonymous target: %d", w.Code)
	}
	w := request(http.MethodGet, "/api/verification/target", token, nil)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("target: %d %s", w.Code, w.Body.String())
	}
	var target map[string]string
	if json.Unmarshal(w.Body.Bytes(), &target) != nil || len(target) != 2 || target["suite"] != modelVerificationSuite || len(target["runtime"]) != 64 {
		t.Fatal("unexpected verification target fields")
	}
	if w := request(http.MethodPost, "/api/console/model-verifications", token, testModelVerification(t)); w.Code != 403 {
		t.Fatalf("operator API key published claims without a live session: %d", w.Code)
	}
}

func TestModelVerificationRejectsInvalidEvidence(t *testing.T) {
	for _, scenario := range []string{"wrong_runtime", "old", "future", "unknown_model", "wrong_provider", "missing_check", "unknown_check", "unknown_code", "fake_codex", "unknown_field", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			h, _, _, _, operator, csrf := memberQuotaFixture(t)
			v := testModelVerification(t)
			switch scenario {
			case "wrong_runtime":
				v.Runtime = strings.Repeat("a", 64)
			case "old":
				v.FinishedAt = time.Now().Add(-8 * 24 * time.Hour)
				v.StartedAt = v.FinishedAt.Add(-time.Minute)
			case "future":
				v.FinishedAt = time.Now().Add(time.Hour)
				v.StartedAt = v.FinishedAt.Add(-time.Minute)
			case "unknown_model":
				v.Model = "arbitrary-model"
			case "wrong_provider":
				v.Provider = AccountTypeCodex
			case "missing_check":
				delete(v.Checks, "tools")
			case "unknown_check":
				v.Checks["other"] = modelVerificationCheck{"passed", "file_edit_verified"}
			case "unknown_code":
				v.Checks["tools"] = modelVerificationCheck{"passed", "http_200"}
			case "fake_codex":
				v.Codex = "unavailable"
			case "oversized":
				v.Model = strings.Repeat("a", 9000)
			}
			var payload any = v
			if scenario == "unknown_field" {
				raw, _ := json.Marshal(v)
				var fields map[string]any
				_ = json.Unmarshal(raw, &fields)
				fields["access_token"] = "must-not-be-stored"
				payload = fields
			}
			w := postModelVerification(t, h, operator, csrf, payload)
			if w.Code != 400 && w.Code != 409 {
				t.Fatalf("accepted invalid evidence: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestModelVerificationExpiryAndCorruption(t *testing.T) {
	h, id, member, _, _, _ := memberQuotaFixture(t)
	saveQuotaPolicy(t, h, id, ClientPolicy{})
	for _, scenario := range []string{"runtime_changed", "expired", "corrupt"} {
		v := testModelVerification(t)
		if scenario == "runtime_changed" {
			v.Runtime = strings.Repeat("a", 64)
		}
		if scenario == "expired" {
			v.FinishedAt = time.Now().Add(-8 * 24 * time.Hour)
			v.StartedAt = v.FinishedAt.Add(-time.Minute)
		}
		if err := h.passport.db.Update(func(tx *bbolt.Tx) error {
			bucket := tx.Bucket([]byte(bucketModelVerifications))
			if scenario == "corrupt" {
				return bucket.Put([]byte(resourceKey(v.Provider, v.Model)), []byte(`{}`))
			}
			return putJSON(bucket, resourceKey(v.Provider, v.Model), v)
		}); err != nil {
			t.Fatal(err)
		}
		if scenario == "corrupt" {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, passportContributionRequest("GET", "/api/me/quotas", member, "", nil))
			if w.Code != 503 {
				t.Fatal("corrupt evidence did not fail closed")
			}
		} else {
			for _, row := range quotaHTTP(t, h, member) {
				if row.Provider == "claude" && (row.Compatibility == nil || !row.Compatibility.Expired) {
					t.Fatal("outdated evidence retained its passed status")
				}
			}
		}
	}
}

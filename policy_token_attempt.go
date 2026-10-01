package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

type policyTokenAttempt struct {
	holds                            []policyBudgetHold
	bound, tokens, expected          int64
	known, complete, failed, settled bool
}

func (a *policyAdmission) hasTokenBudget() bool {
	if a == nil {
		return false
	}
	for _, hold := range a.holds {
		if hold.Limits.DailyTokens > 0 || hold.Limits.MonthlyTokens > 0 {
			return true
		}
	}
	return false
}

func unboundedPolicyRequest() error {
	return &policyError{Status: 422, Code: "policy_token_bound_unavailable", Message: "token budgets require a bounded Codex Responses or Claude Messages request"}
}

func policyRequestTokenBound(r *http.Request, provider AccountType) (int64, error) {
	if r.GetBody == nil || (provider != AccountTypeCodex && provider != AccountTypeClaude) {
		return 0, unboundedPolicyRequest()
	}
	if provider == AccountTypeCodex && !strings.HasSuffix(r.URL.Path, "/responses") {
		return 0, unboundedPolicyRequest()
	}
	if provider == AccountTypeClaude && !strings.HasSuffix(r.URL.Path, "/messages") {
		return 0, unboundedPolicyRequest()
	}
	body, err := r.GetBody()
	if err != nil {
		return 0, err
	}
	defer body.Close()
	var request struct {
		Model      string `json:"model"`
		N          *int   `json:"n"`
		Background bool   `json:"background"`
		Tools      []struct {
			Type string `json:"type"`
		} `json:"tools"`
	}
	if json.NewDecoder(body).Decode(&request) != nil || request.Background || (request.N != nil && *request.N != 1) {
		return 0, unboundedPolicyRequest()
	}
	for _, tool := range request.Tools {
		if tool.Type != "function" && !(provider == AccountTypeClaude && tool.Type == "") {
			return 0, unboundedPolicyRequest()
		}
	}
	// Reserve extended-context capacity even when the request uses a smaller profile.
	for _, model := range poolModels {
		if model.ID != request.Model || model.AccountType != provider || model.ContextWindow <= 0 || model.MaxTokens <= 0 {
			continue
		}
		context := model.ContextWindow
		if context < 1000000 {
			context = 1000000
		}
		return int64(context) + int64(model.MaxTokens), nil
	}
	return 0, unboundedPolicyRequest()
}

func (a *policyAdmission) reserveTokenAttempt(bound int64) (*policyTokenAttempt, error) {
	return a.reserveTokenAttemptAt(bound, time.Now().UTC())
}

func (a *policyAdmission) reserveTokenAttemptAt(bound int64, now time.Time) (*policyTokenAttempt, error) {
	if bound <= 0 {
		return nil, unboundedPolicyRequest()
	}
	a.attemptMu.Lock()
	defer a.attemptMu.Unlock()
	p := a.store
	p.policyMu.Lock()
	defer p.policyMu.Unlock()
	attempt := &policyTokenAttempt{bound: bound}
	err := p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketPassportPolicyUsage))
		if b == nil {
			return errors.New("policy usage unavailable")
		}
		for _, hold := range a.holds {
			for _, key := range []string{hold.Day, hold.Month} {
				c, err := readPolicyCounter(b, key)
				if err != nil {
					return err
				}
				if c.ReservedTokens < hold.Tokens {
					return errors.New("policy reservation underflow")
				}
				c.ReservedTokens -= hold.Tokens
				if err := writePolicyCounter(b, key, c); err != nil {
					return err
				}
			}
			copy := hold
			_, copy.Day, copy.Month = policyUsageKeys(hold.Scope, now)
			copy.Tokens = bound
			for _, key := range []string{copy.Day, copy.Month} {
				c, err := readPolicyCounter(b, key)
				if err != nil {
					return err
				}
				limit := hold.Limits.MonthlyTokens
				code := "policy_monthly_tokens_exceeded"
				if key == copy.Day {
					limit = hold.Limits.DailyTokens
					code = "policy_daily_tokens_exceeded"
				}
				if limit > 0 && (c.Tokens > limit || c.ReservedTokens > limit-c.Tokens || bound > limit-c.Tokens-c.ReservedTokens) {
					return &policyError{Status: 429, Code: code, Message: "insufficient token budget for the upstream consumption ceiling"}
				}
				c.ReservedTokens += bound
				if err := writePolicyCounter(b, key, c); err != nil {
					return err
				}
			}
			attempt.holds = append(attempt.holds, copy)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range a.holds {
		p.policyReserved[a.holds[i].Scope] += bound - a.holds[i].Tokens
		a.holds[i].Tokens = 0
	}
	a.attempt = attempt
	return attempt, nil
}

func (a *policyAdmission) recordAttemptUsage(tokens int64, durable bool) {
	a.attemptMu.Lock()
	defer a.attemptMu.Unlock()
	if a.attempt == nil {
		return
	}
	if !durable || tokens < 0 {
		a.attempt.failed = true
		return
	}
	a.attempt.known = true
	a.attempt.tokens += tokens
	a.settleTokenAttempt(a.attempt)
}

func (a *policyAdmission) settleTokenAttempt(attempt *policyTokenAttempt) {
	if attempt.settled || attempt.failed || !attempt.known || !attempt.complete || attempt.tokens < attempt.expected {
		return
	}
	if attempt.tokens > attempt.bound {
		attempt.failed = true
		log.Printf("policy: upstream exceeded its token ceiling; reservation retained")
		return
	}
	p := a.store
	p.policyMu.Lock()
	defer p.policyMu.Unlock()
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketPassportPolicyUsage))
		if b == nil {
			return errors.New("policy usage unavailable")
		}
		for _, hold := range attempt.holds {
			for _, key := range []string{hold.Day, hold.Month} {
				c, err := readPolicyCounter(b, key)
				if err != nil {
					return err
				}
				if c.ReservedTokens < hold.Tokens {
					return errors.New("policy reservation underflow")
				}
				c.ReservedTokens -= hold.Tokens
				c.Tokens += attempt.tokens
				if err := writePolicyCounter(b, key, c); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		attempt.failed = true
		log.Printf("policy: token settlement failed: %v", err)
		return
	}
	for _, hold := range attempt.holds {
		p.policyReserved[hold.Scope] -= hold.Tokens
	}
	attempt.settled = true
}

type policyCompletionBody struct {
	io.ReadCloser
	admission               *policyAdmission
	attempt                 *policyTokenAttempt
	json                    bool
	tail                    []byte
	terminal                bool
	settleReported          bool
	invalid                 bool
	expected                int64
	input                   int64
	inputKnown, outputKnown bool
	mu                      sync.Mutex
}

func strictUsageTokens(obj map[string]any) (int64, bool) {
	u, ok := obj["usage"].(map[string]any)
	if !ok {
		return 0, false
	}
	input, inputOK := strictTokenNumber(u["input_tokens"])
	output, outputOK := strictTokenNumber(u["output_tokens"])
	if !inputOK || !outputOK || input > math.MaxInt64-output {
		return 0, false
	}
	cached := int64(0)
	if details, ok := u["input_tokens_details"].(map[string]any); ok {
		if value, exists := details["cached_tokens"]; exists {
			var valid bool
			cached, valid = strictTokenNumber(value)
			if !valid {
				return 0, false
			}
		}
	} else if value, exists := u["cached_input_tokens"]; exists {
		var valid bool
		cached, valid = strictTokenNumber(value)
		if !valid {
			return 0, false
		}
	}
	if cached > input {
		return 0, false
	}
	return input - cached + output, true
}

func strictTokenNumber(value any) (int64, bool) {
	f, ok := value.(float64)
	if !ok || f < 0 || f >= math.MaxInt64 || math.Trunc(f) != f {
		return 0, false
	}
	return int64(f), true
}

func (b *policyCompletionBody) inspect(data []byte) {
	var obj map[string]any
	if json.Unmarshal(data, &obj) != nil {
		return
	}
	typeName, _ := obj["type"].(string)
	switch typeName {
	case "response.completed":
		response, _ := obj["response"].(map[string]any)
		b.expected, b.terminal = strictUsageTokens(response)
	case "message_start":
		message, _ := obj["message"].(map[string]any)
		usage, _ := message["usage"].(map[string]any)
		b.input, b.inputKnown = strictTokenNumber(usage["input_tokens"])
	case "message_delta":
		usage, _ := obj["usage"].(map[string]any)
		output, ok := strictTokenNumber(usage["output_tokens"])
		b.outputKnown = ok && b.inputKnown && b.input <= math.MaxInt64-output
		if b.outputKnown {
			b.expected = b.input + output
		}
	case "message_stop":
		b.terminal = b.inputKnown && b.outputKnown
	}
}

func (b *policyCompletionBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.ReadCloser.Read(p)
	if len(b.tail)+n > 8<<20 {
		b.invalid = true
		b.tail = nil
	}
	if !b.invalid {
		b.tail = append(b.tail, p[:n]...)
		if !b.json {
			for {
				end := bytes.IndexByte(b.tail, '\n')
				if end < 0 {
					break
				}
				line := bytes.TrimSpace(b.tail[:end])
				if bytes.HasPrefix(line, []byte("data:")) {
					b.inspect(bytes.TrimSpace(line[5:]))
				}
				b.tail = b.tail[end+1:]
			}
		} else if err == io.EOF {
			var obj map[string]any
			if json.Unmarshal(b.tail, &obj) == nil {
				b.expected, b.terminal = strictUsageTokens(obj)
			}
		}
	}
	if (err == io.EOF || !b.json) && b.terminal && !b.invalid {
		b.admission.attemptMu.Lock()
		b.attempt.complete = true
		b.attempt.expected = b.expected
		if b.settleReported {
			b.attempt.known = true
			b.attempt.tokens = b.expected
		}
		b.admission.settleTokenAttempt(b.attempt)
		b.admission.attemptMu.Unlock()
	}
	return n, err
}

func (h *proxyHandler) clientPolicyRoundTrip(r *http.Request, provider AccountType, admission *policyAdmission) (*http.Response, error) {
	if !admission.hasTokenBudget() {
		return h.transport.RoundTrip(r)
	}
	bound, err := policyRequestTokenBound(r, provider)
	if err != nil {
		return nil, err
	}
	attempt, err := admission.reserveTokenAttempt(bound)
	if err != nil {
		return nil, err
	}
	resp, err := h.transport.RoundTrip(r)
	if err == nil && resp != nil && resp.Body != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		resp.Body = &policyCompletionBody{ReadCloser: resp.Body, admission: admission, attempt: attempt, json: strings.Contains(resp.Header.Get("Content-Type"), "application/json")}
	}
	return resp, err
}

package main

import (
	"encoding/json"
	"fmt"
	"testing"

	"codex-pool-proxy/internal/runnerproto"
)

func TestSessionHTTPTurnAndApprovalSignatureLimits(t *testing.T) {
	for _, count := range []int{16, 17, 32, 33} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			t.Setenv("POOL_JWT_SECRET", "synthetic-signature-limit-secret")
			h, principal, _, _, _, _ := memberQuotaFixture(t)
			saveQuotaPolicy(t, h, principal, ClientPolicy{Providers: PolicySelector{Allow: []string{"*"}}})
			login, err := h.passport.createAppToken(h.passport.principal(principal), getPoolJWTSecret())
			if err != nil {
				t.Fatal(err)
			}
			b, err := h.sessionBroker()
			if err != nil {
				t.Fatal(err)
			}
			r := brokerRunner(t, b, principal)
			created := appAuthRequest(h, "POST", "/api/sessions", login.Token, `{"agent":"codex","model":"gpt-5.6-sol","permission_mode":"gated","workspace":{"mode":"ephemeral"}}`)
			if created.Code != 201 {
				t.Fatalf("create=%d %s", created.Code, created.Body.String())
			}
			var s runnerproto.Session
			if err := json.Unmarshal(created.Body.Bytes(), &s); err != nil {
				t.Fatal(err)
			}
			if err := b.claim(r, s.ID); err != nil {
				t.Fatal(err)
			}
			brokerEvent(t, b, r, s.ID, "workspace.status", func(m *runnerproto.Message) { m.Status = "ready" })
			signatures := map[string]string{r.hello.CommandKeyID: runnerproto.SignCommand([]byte("own key"), []byte("input"))}
			for i := 1; i < count; i++ {
				key := []byte(fmt.Sprintf("synthetic-key-%d", i))
				signatures[runnerproto.CommandKeyID(key)] = runnerproto.SignCommand(key, []byte("input"))
			}
			input := runnerproto.TurnInput{Input: "synthetic input", IssuedAt: b.now().UnixMilli(), Signatures: signatures}
			body, _ := json.Marshal(input)
			response := appAuthRequest(h, "POST", "/api/sessions/"+s.ID+"/turns", login.Token, string(body))
			if count <= 32 && response.Code != 200 {
				t.Fatalf("turn=%d %s", response.Code, response.Body.String())
			}
			if count > 32 {
				if response.Code != 400 {
					t.Fatalf("over-limit turn=%d", response.Code)
				}
				// Create a valid turn so the approval boundary is tested independently.
				input.Signatures = map[string]string{r.hello.CommandKeyID: signatures[r.hello.CommandKeyID]}
				body, _ = json.Marshal(input)
				if response = appAuthRequest(h, "POST", "/api/sessions/"+s.ID+"/turns", login.Token, string(body)); response.Code != 200 {
					t.Fatal(response.Body.String())
				}
			}
			brokerEvent(t, b, r, s.ID, "turn.started", nil)
			brokerEvent(t, b, r, s.ID, "approval.request", func(m *runnerproto.Message) { m.ApprovalID, m.Command = "approval-1", "synthetic command" })
			body, _ = json.Marshal(runnerproto.ApprovalDecision{Decision: "approve", IssuedAt: input.IssuedAt, Signatures: signatures})
			response = appAuthRequest(h, "POST", "/api/sessions/"+s.ID+"/approvals/approval-1", login.Token, string(body))
			want := 204
			if count > 32 {
				want = 400
			}
			if response.Code != want {
				t.Fatalf("approval=%d %s, want %d", response.Code, response.Body.String(), want)
			}
		})
	}
}

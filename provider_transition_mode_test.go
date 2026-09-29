package main

import (
	"strings"
	"testing"
)

func TestTransitionModesFollowCapabilities(t *testing.T) {
	tests := []struct {
		from AccountType
		to   AccountType
		want TransitionMode
	}{
		{AccountTypeCodex, AccountTypeCodex, TransitionNative},
		{AccountTypeAntigravity, AccountTypeAntigravity, TransitionNative},
		{AccountTypeClaude, AccountTypeZAI, TransitionFullHistory},
		{AccountTypeZAI, AccountTypeClaude, TransitionFullHistory},
		{AccountTypeCodex, AccountTypeAntigravity, TransitionSafeHistory},
		{AccountTypeAntigravity, AccountTypeCodex, TransitionSafeHistory},
		{AccountTypeClaude, AccountTypeAntigravity, TransitionSafeHistory},
		{AccountTypeZAI, AccountTypeAntigravity, TransitionSafeHistory},
		{AccountTypeCodex, AccountType("unregistered"), TransitionSummary},
	}
	for _, test := range tests {
		if got := transitionMode(test.from, test.to); got != test.want {
			t.Errorf("%s -> %s mode=%s want=%s", test.from, test.to, got, test.want)
		}
	}
}

func TestTransitionModeAppliedToHandoff(t *testing.T) {
	for _, test := range []struct {
		from AccountType
		to   AccountType
		want TransitionMode
	}{
		{AccountTypeClaude, AccountTypeZAI, TransitionFullHistory},
		{AccountTypeCodex, AccountTypeAntigravity, TransitionSafeHistory},
		{AccountTypeCodex, AccountType("unregistered"), TransitionSummary},
	} {
		t.Run(string(test.from)+"_to_"+string(test.to), func(t *testing.T) {
			store := newConversationHandoffStore()
			fromFormat := transitionFormat(test.from)
			toFormat := transitionFormat(test.to)
			initial := transitionBody(t, fromFormat, []Message{
				transitionText("user", "old request"), transitionText("assistant", "old visible answer"),
			})
			if _, _, err := store.Prepare(conversationScopedKey("user", "mode-conversation"), test.from, contextTestPath(fromFormat), initial); err != nil {
				t.Fatal(err)
			}
			request := addContextOpaqueState(t, transitionBody(t, toFormat, []Message{transitionText("user", "new request")}))
			out, result, err := store.Prepare(conversationScopedKey("user", "mode-conversation"), test.to, contextTestPath(toFormat), request)
			if err != nil || !result.Switched || result.Mode != test.want || result.From != test.from {
				t.Fatalf("handoff result=%+v err=%v", result, err)
			}
			if strings.Contains(string(out), "resp_previous_provider") || strings.Contains(string(out), "opaque-reasoning") {
				t.Fatalf("opaque state leaked: %s", out)
			}
			visible := normalizedContextText(t, contextTestPath(toFormat), out)
			if !strings.Contains(visible, "old visible answer") || !strings.Contains(visible, "new request") {
				t.Fatalf("visible context lost: %q", visible)
			}
			if test.want == TransitionSummary && !strings.Contains(visible, "Earlier conversation summary") {
				t.Fatalf("summary mode did not render summary: %q", visible)
			}
		})
	}
}

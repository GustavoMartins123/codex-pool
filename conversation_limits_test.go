package main

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPinQuotasSharePrincipalAcrossCredentialsAndPreserveOtherOwners(t *testing.T) {
	p := newPoolState(nil, false)
	for i := 0; i < maxPinsPerPrincipal; i++ {
		owner := fmt.Sprintf("alice-c-client%d", i%2)
		if err := p.pinForUser(owner, conversationPinKey(owner, fmt.Sprint(i)), "a"); err != nil {
			t.Fatal(err)
		}
	}
	err := p.pinForUser("alice-c-third", conversationPinKey("alice-c-third", "extra"), "a")
	requirePolicyCode(t, err, "conversation_pin_quota_exceeded")
	if err := p.pinForUser("bob", "bob-conversation", "a"); err != nil {
		t.Fatal(err)
	}
	if len(p.convPin) != maxPinsPerPrincipal+1 {
		t.Fatal("quota evicted live pins")
	}
	requirePolicyCode(t, p.pinForUser("bob", strings.Repeat("x", maxPinBytes), "a"), "conversation_pin_too_large")
}

func TestPinByteQuotaAndExpiredEntries(t *testing.T) {
	p := newPoolState(nil, false)
	owner := "alice"
	for i := 0; i < 18; i++ {
		if err := p.pinForUser(owner, fmt.Sprint(i)+strings.Repeat("x", 1700), "a"); err != nil {
			t.Fatal(err)
		}
	}
	requirePolicyCode(t, p.pinForUser(owner, "extra"+strings.Repeat("x", 1700), "a"), "conversation_pin_quota_exceeded")
	p.mu.Lock()
	for key := range p.convUpdatedAt {
		p.convUpdatedAt[key] = time.Now().Add(-conversationPinTTL)
	}
	p.mu.Unlock()
	if err := p.pinForUser(owner, "fresh", "a"); err != nil {
		t.Fatal(err)
	}
	if len(p.convPin) != 1 || len(p.convOwner) != 1 || len(p.convUpdatedAt) != 1 {
		t.Fatal("expired pins retained")
	}
}

func TestContextCountQuotaTTLAndReadIsolation(t *testing.T) {
	s := newConversationHandoffStore()
	body := contextTestBody(contextFormatResponses, "hello", false)
	for i := 0; i < maxContextsPerPrincipal; i++ {
		key := conversationScopedKey(fmt.Sprintf("alice-c-client%d", i%2), fmt.Sprint(i))
		if _, _, err := s.Prepare(key, AccountTypeCodex, "/v1/responses", body); err != nil {
			t.Fatal(err)
		}
	}
	key := conversationScopedKey("alice-c-third", "extra")
	out, result, err := s.Prepare(key, AccountTypeCodex, "/v1/responses", body)
	if err != nil {
		t.Fatalf("quota must degrade the request, got: %v", err)
	}
	if string(out) != string(body) || len(result.Warnings) == 0 {
		t.Fatal("quota overflow must pass through with a warning")
	}
	if _, retained := func() (conversationHandoffRecord, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		r, ok := s.records[key]
		return r, ok
	}(); retained {
		t.Fatal("quota bypassed by third credential: record retained anyway")
	}
	bob := conversationScopedKey("bob", "safe")
	if _, _, err := s.Prepare(bob, AccountTypeCodex, "/v1/responses", body); err != nil {
		t.Fatal(err)
	}
	state, _ := s.State(bob)
	state.Messages[0].Parts[0].Text = "mutated"
	state.ProviderSessions[AccountTypeCodex].NativeSessionID = "mutated"
	fresh, _ := s.State(bob)
	if fresh.Messages[0].Parts[0].Text == "mutated" || fresh.ProviderSessions[AccountTypeCodex].NativeSessionID == "mutated" {
		t.Fatal("read exposed retained state")
	}
	s.mu.Lock()
	for k, v := range s.records {
		v.State.UpdatedAt = time.Now().Add(-conversationMemoryTTL)
		s.records[k] = v
	}
	s.mu.Unlock()
	if _, ok := s.State(bob); ok {
		t.Fatal("expired state readable")
	}
	if _, _, err := s.Prepare(key, AccountTypeCodex, "/v1/responses", body); err != nil {
		t.Fatal(err)
	}
	if len(s.records) != 1 {
		t.Fatal("expired contexts retained")
	}
}

func TestContextBytesRefuseWritesAndFaultAsyncRetention(t *testing.T) {
	s := newConversationHandoffStore()
	for i := 0; i < 5; i++ {
		record := conversationHandoffRecord{State: ConversationState{UpdatedAt: time.Now(), Summary: strings.Repeat("a", 3<<20)}}
		if err := s.saveLocked(conversationScopedKey("alice", fmt.Sprint(i)), record); err != nil {
			t.Fatal(err)
		}
	}
	record := conversationHandoffRecord{State: ConversationState{UpdatedAt: time.Now(), Summary: strings.Repeat("b", 3<<20)}}
	if err := s.saveLocked(conversationScopedKey("alice", "sixth"), record); err == nil {
		t.Fatal("byte quota bypassed")
	}
	if err := s.saveLocked(conversationScopedKey("bob", "first"), record); err != nil {
		t.Fatal(err)
	}
	key := conversationScopedKey("alice", "0")
	record.State.Summary = strings.Repeat("x", maxContextBytes)
	if err := s.saveOutcomeLocked(key, record); err == nil {
		t.Fatal("oversize outcome accepted")
	}
	body := contextTestBody(contextFormatResponses, "next", false)
	if out, _, err := s.Prepare(key, AccountTypeCodex, "/v1/responses", body); err != nil || string(out) != string(body) {
		t.Fatalf("failed retention must degrade to pass-through, got err=%v", err)
	}
	s.mu.Lock()
	fault := s.records[key].Fault
	s.mu.Unlock()
	if fault != "" {
		t.Fatal("faulted record must self-heal on the next request")
	}
	if out, result, err := s.Prepare(conversationScopedKey("other", "huge"), AccountTypeCodex, "/v1/responses", []byte(strings.Repeat("x", maxContextBytes+1))); err != nil || len(result.Warnings) == 0 {
		t.Fatalf("oversize input must pass through with a warning, got err=%v", err)
	} else if string(out) != strings.Repeat("x", maxContextBytes+1) {
		t.Fatal("oversize input must be returned unrewritten")
	}
}

func TestContextConcurrentWritesRemainWithinQuota(t *testing.T) {
	s := newConversationHandoffStore()
	body := contextTestBody(contextFormatResponses, "hello", false)
	var workers sync.WaitGroup
	for i := 0; i < maxContextsPerPrincipal*2; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			s.Prepare(conversationScopedKey("alice", fmt.Sprint(i)), AccountTypeCodex, "/v1/responses", body)
		}(i)
	}
	workers.Wait()
	if len(s.records) != maxContextsPerPrincipal {
		t.Fatalf("records = %d", len(s.records))
	}
}

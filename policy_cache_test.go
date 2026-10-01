package main

import (
	"fmt"
	"testing"
	"time"
)

func TestPolicyCacheRevisionsConfigurationStatusExpiryAndCopyIsolation(t *testing.T) {
	p, c := testPolicyPassport(t)
	configured := map[string]ClientPolicy{"*": {Models: PolicySelector{Allow: []string{"gpt-5.5"}}}}
	_, policy, _, err := p.evaluateClientPolicy(c.PrincipalID, c.ID, configured)
	if err != nil {
		t.Fatal(err)
	}
	policy.Models.Allow[0] = "mutated"
	_, policy, _, err = p.evaluateClientPolicy(c.PrincipalID, c.ID, configured)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Models.Allow[0] != "gpt-5.5" || p.policyCache.hits != 1 {
		t.Fatal("cached policy mutated or missed")
	}
	p.mu.Lock()
	p.principals[c.PrincipalID].PolicyRevision++
	p.clients[c.ID].Policy.Models.Deny = []string{"gpt-5.5"}
	p.mu.Unlock()
	_, policy, _, err = p.evaluateClientPolicy(c.PrincipalID, c.ID, configured)
	if err != nil {
		t.Fatal(err)
	}
	if policyAllows(policy.Models, "gpt-5.5") {
		t.Fatal("revision change ignored")
	}
	configured["*"] = ClientPolicy{Models: PolicySelector{Deny: []string{"other"}}}
	_, policy, _, err = p.evaluateClientPolicy(c.PrincipalID, c.ID, configured)
	if err != nil {
		t.Fatal(err)
	}
	if policyAllows(policy.Models, "other") {
		t.Fatal("configuration change ignored")
	}
	p.mu.Lock()
	p.clients[c.ID].Status = "revoked"
	p.mu.Unlock()
	if _, _, _, err = p.evaluateClientPolicy(c.PrincipalID, c.ID, configured); err == nil {
		t.Fatal("cache bypassed revocation")
	}
	p.mu.Lock()
	p.clients[c.ID].Status = "active"
	past := time.Now().Add(-time.Second)
	p.clients[c.ID].ExpiresAt = &past
	p.mu.Unlock()
	if _, _, _, err = p.evaluateClientPolicy(c.PrincipalID, c.ID, configured); err == nil {
		t.Fatal("cache bypassed expiry")
	}
	configured["missing-client"] = ClientPolicy{}
	if _, _, _, err = p.evaluateClientPolicy(c.PrincipalID, c.ID, configured); err == nil {
		t.Fatal("unknown policy binding cached")
	}
}

func TestPolicyCacheIsBounded(t *testing.T) {
	p := &PassportStore{}
	for i := 0; i < policyCacheMaxEntries+20; i++ {
		pr := &Principal{ID: fmt.Sprint(i), Kind: PrincipalMember, PolicyRevision: uint64(i)}
		if _, err := p.cachedEffectivePolicy(pr, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.policyCache.entries) != policyCacheMaxEntries || p.policyCache.bytes > policyCacheMaxBytes {
		t.Fatal("cache exceeded capacity")
	}
}

func BenchmarkEffectivePolicy(b *testing.B) {
	p := &PassportStore{}
	principal := &Principal{ID: "member", Kind: PrincipalMember, PolicyRevision: 1}
	client := &ClientCredential{ID: "client", Policy: ClientPolicy{Models: PolicySelector{Deny: []string{"blocked"}}}}
	models := make([]string, 100)
	for i := range models {
		models[i] = fmt.Sprintf("model-%d", i)
	}
	configured := map[string]ClientPolicy{"*": {Models: PolicySelector{Allow: models}}, "role:member": {Models: PolicySelector{Allow: models}}}
	sources := policySourcesFor(principal, client, configured)
	b.Run("cached", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := p.cachedEffectivePolicy(principal, client, configured); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("uncached", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := combinePolicySources(sources, principal.Kind); err != nil {
				b.Fatal(err)
			}
		}
	})
}

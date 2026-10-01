package main

import (
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"errors"
)

const policyCacheMaxEntries = 1024
const policyCacheMaxBytes = 4 << 20

type policyCacheKey struct {
	Principal, Client string
	Revision          uint64
	Kind              PrincipalKind
	Sources           [32]byte
}

type policyCacheEntry struct {
	key    policyCacheKey
	policy ClientPolicy
	bytes  int
}

type policyCache struct {
	entries map[policyCacheKey]*list.Element
	order   *list.List
	bytes   int
	hits    uint64
}

func cloneClientPolicy(policy ClientPolicy) ClientPolicy {
	policy.Models.Allow = append([]string(nil), policy.Models.Allow...)
	policy.Models.Deny = append([]string(nil), policy.Models.Deny...)
	policy.Providers.Allow = append([]string(nil), policy.Providers.Allow...)
	policy.Providers.Deny = append([]string(nil), policy.Providers.Deny...)
	return policy
}

func (p *PassportStore) cachedEffectivePolicy(principal *Principal, client *ClientCredential, configured map[string]ClientPolicy) (ClientPolicy, error) {
	sources := policySourcesFor(principal, client, configured)
	raw, err := json.Marshal(sources)
	if err != nil {
		return ClientPolicy{}, err
	}
	if len(raw) > policyCacheMaxBytes {
		return ClientPolicy{}, errors.New("effective policy exceeds cache capacity")
	}
	key := policyCacheKey{Principal: principal.ID, Revision: principal.PolicyRevision, Kind: principal.Kind, Sources: sha256.Sum256(raw)}
	if client != nil {
		key.Client = client.ID
	}
	p.policyCacheMu.Lock()
	defer p.policyCacheMu.Unlock()
	if p.policyCache == nil {
		p.policyCache = &policyCache{entries: map[policyCacheKey]*list.Element{}, order: list.New()}
	}
	c := p.policyCache
	if element := c.entries[key]; element != nil {
		c.order.MoveToFront(element)
		c.hits++
		return cloneClientPolicy(element.Value.(policyCacheEntry).policy), nil
	}
	policy, err := combinePolicySources(sources, principal.Kind)
	if err != nil {
		return ClientPolicy{}, err
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return ClientPolicy{}, err
	}
	entryBytes := len(encoded) + len(key.Principal) + len(key.Client) + 256
	if entryBytes > policyCacheMaxBytes {
		return ClientPolicy{}, errors.New("effective policy exceeds cache capacity")
	}
	for len(c.entries) >= policyCacheMaxEntries || c.bytes+entryBytes > policyCacheMaxBytes {
		element := c.order.Back()
		entry := element.Value.(policyCacheEntry)
		delete(c.entries, entry.key)
		c.bytes -= entry.bytes
		c.order.Remove(element)
	}
	c.entries[key] = c.order.PushFront(policyCacheEntry{key: key, policy: cloneClientPolicy(policy), bytes: entryBytes})
	c.bytes += entryBytes
	return policy, nil
}

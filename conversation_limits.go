package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

const (
	conversationMemoryTTL       = 6 * time.Hour
	maxPinsPerPrincipal         = 128
	maxPinBytes                 = 4096
	maxPinBytesPerPrincipal     = 64 << 10
	maxPinBytesTotal            = 4 << 20
	maxContextBytes             = 4 << 20
	maxContextBytesPerPrincipal = 16 << 20
	maxContextBytesTotal        = 128 << 20
	maxContextsPerPrincipal     = 64
)

func conversationQuotaOwner(identity string) string {
	principal, _ := splitClientIdentity(identity)
	return principal
}

func pinIdentity(owner, key string) string {
	if owner != "" {
		return owner
	}
	if head, _, ok := strings.Cut(key, "\x00"); ok {
		if strings.HasPrefix(head, "antigravity:") {
			parts := strings.SplitN(head, ":", 3)
			if len(parts) == 3 {
				head = parts[2]
			}
		}
		return head
	}
	return ""
}

func pinSize(owner, key, account string) int { return len(owner) + 2*len(key) + len(account) + 192 }

func (p *poolState) checkPinLocked(owner, key, account string, now time.Time) error {
	p.evictOldestPinsLocked(now)
	size := pinSize(owner, key, account)
	if size > maxPinBytes {
		return accountControlError("conversation_pin_too_large", 413)
	}
	principal := conversationQuotaOwner(pinIdentity(owner, key))
	count, ownedBytes, totalBytes := 1, size, size
	for id, acc := range p.convPin {
		if id == key {
			continue
		}
		bytes := pinSize(p.convOwner[id], id, acc)
		totalBytes += bytes
		if conversationQuotaOwner(pinIdentity(p.convOwner[id], id)) == principal {
			count++
			ownedBytes += bytes
		}
	}
	_, exists := p.convPin[key]
	if count > maxPinsPerPrincipal || ownedBytes > maxPinBytesPerPrincipal {
		return accountControlError("conversation_pin_quota_exceeded", 429)
	}
	if !exists && len(p.convPin) >= maxConversationPins || totalBytes > maxPinBytesTotal {
		return accountControlError("conversation_pin_capacity_exceeded", 503)
	}
	return nil
}

func cloneHandoffRecord(record conversationHandoffRecord) (conversationHandoffRecord, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return conversationHandoffRecord{}, err
	}
	var copy conversationHandoffRecord
	err = json.Unmarshal(raw, &copy)
	copy.Bytes = record.Bytes
	return copy, err
}

func (s *conversationHandoffStore) pruneLocked(now time.Time) {
	for key, record := range s.records {
		if !record.State.UpdatedAt.Add(conversationMemoryTTL).After(now) {
			delete(s.records, key)
		}
	}
}

func (s *conversationHandoffStore) saveLocked(key conversationKey, record conversationHandoffRecord) error {
	if len(key.owner)+len(key.conversationID) > maxPinBytes {
		return fmt.Errorf("conversation identity exceeds memory limit")
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	record.Bytes = len(raw) + len(key.owner) + len(key.conversationID) + 512
	if record.Bytes > maxContextBytes {
		return fmt.Errorf("conversation exceeds byte limit")
	}
	owner := conversationQuotaOwner(key.owner)
	count, ownedBytes, totalBytes := 1, record.Bytes, record.Bytes
	for other, existing := range s.records {
		if other == key {
			continue
		}
		totalBytes += existing.Bytes
		if conversationQuotaOwner(other.owner) == owner {
			count++
			ownedBytes += existing.Bytes
		}
	}
	if count > maxContextsPerPrincipal || ownedBytes > maxContextBytesPerPrincipal {
		return fmt.Errorf("conversation memory quota exceeded")
	}
	_, exists := s.records[key]
	if !exists && len(s.records) >= contextHandoffMaxConversations || totalBytes > maxContextBytesTotal {
		return fmt.Errorf("conversation memory capacity exceeded")
	}
	s.records[key] = record
	return nil
}

func (s *conversationHandoffStore) saveOutcomeLocked(key conversationKey, record conversationHandoffRecord) error {
	if err := s.saveLocked(key, record); err != nil {
		if previous, ok := s.records[key]; ok {
			previous.Fault = err.Error()
			s.records[key] = previous
		}
		log.Printf("conversation retention failed: %v", err)
		return err
	}
	return nil
}

func (h *proxyHandler) startConversationCleanup(ctx context.Context) {
	store := h.getContextHandoff()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				h.pool.mu.Lock()
				h.pool.evictOldestPinsLocked(now)
				h.pool.mu.Unlock()
				store.mu.Lock()
				store.pruneLocked(now)
				store.mu.Unlock()
			}
		}
	}()
}

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/bbolt"
)

const analyticsGapsKey = "accounting_gaps"

func (s *usageStore) loadActiveAccountingGapSidecar() {
	if s == nil || s.analyticsGapPath == "" {
		return
	}
	encoded, err := os.ReadFile(s.analyticsGapPath)
	if err != nil {
		return
	}
	var gap AccountingGap
	if json.Unmarshal(encoded, &gap) == nil && !gap.StartedAt.IsZero() && gap.EndedAt == nil {
		s.analyticsGap = &gap
	}
}

// persistActiveAccountingGapSidecar atomically records the active gap so a
// crash between "durable write failed" and "gap closed" cannot lose the
// accounting window. Writers are serialized under analyticsReliabilityMu and
// staged through a unique temp file: a fixed shared ".tmp" name let two
// concurrent writers interleave truncation and payload, producing a mixed
// file that failed to parse at restart — silently discarding the gap.
func (s *usageStore) persistActiveAccountingGapSidecar(gap *AccountingGap) {
	if s == nil || s.analyticsGapPath == "" || gap == nil {
		return
	}
	s.analyticsReliabilityMu.Lock()
	defer s.analyticsReliabilityMu.Unlock()
	s.persistActiveAccountingGapSidecarLocked(gap)
}

// persistActiveAccountingGapSidecarLocked is the locked variant of
// persistActiveAccountingGapSidecar; analyticsReliabilityMu must be held.
func (s *usageStore) persistActiveAccountingGapSidecarLocked(gap *AccountingGap) {
	encoded, err := json.Marshal(gap)
	if err != nil {
		return
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.analyticsGapPath), ".analytics-gap-*.tmp")
	if err != nil {
		return
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return
	}
	if err := temporary.Close(); err != nil {
		return
	}
	_ = os.Rename(name, s.analyticsGapPath)
}

type AccountingGap struct {
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Reason    string     `json:"reason"`
}

func (s *usageStore) configureAnalyticsReserve(path string, bytes int64) error {
	if s == nil || bytes <= 0 {
		return nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	block := make([]byte, 1024*1024)
	for written := int64(0); written < bytes; {
		chunk := int64(len(block))
		if remaining := bytes - written; remaining < chunk {
			chunk = remaining
		}
		n, err := file.Write(block[:chunk])
		if err != nil {
			return err
		}
		written += int64(n)
	}
	if err := file.Sync(); err != nil {
		return err
	}
	s.analyticsReliabilityMu.Lock()
	s.analyticsReservePath = path
	s.analyticsReliabilityMu.Unlock()
	return nil
}

func (s *usageStore) releaseAnalyticsReserve() bool {
	s.analyticsReliabilityMu.Lock()
	path := s.analyticsReservePath
	s.analyticsReservePath = ""
	s.analyticsReliabilityMu.Unlock()
	if path == "" {
		return false
	}
	return os.Remove(path) == nil
}

func (s *usageStore) recordReliably(usage RequestUsage, costUSD float64) error {
	err := s.recordWithCost(usage, costUSD)
	if err == nil {
		s.closeAccountingGap(usage.Timestamp)
		return nil
	}
	if s.releaseAnalyticsReserve() {
		if retryErr := s.recordWithCost(usage, costUSD); retryErr == nil {
			s.closeAccountingGap(usage.Timestamp)
			return nil
		} else {
			err = retryErr
		}
	}
	s.openAccountingGap(usage.Timestamp, err)
	return err
}

func (s *usageStore) loadAccountingGaps(tx *bbolt.Tx) []AccountingGap {
	var gaps []AccountingGap
	if bucket := tx.Bucket([]byte(bucketAnalyticsState)); bucket != nil {
		_ = json.Unmarshal(bucket.Get([]byte(analyticsGapsKey)), &gaps)
	}
	return gaps
}

func (s *usageStore) persistAccountingGap(gap AccountingGap) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketAnalyticsState))
		if bucket == nil {
			return errors.New("analytics state bucket missing")
		}
		gaps := s.loadAccountingGaps(tx)
		gaps = append(gaps, gap)
		encoded, err := json.Marshal(gaps)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(analyticsGapsKey), encoded)
	})
}

// openAccountingGap records the start of an unreliable analytics window.
// The snapshot AND its sidecar write run under one lock hold: a close (or a
// later re-open creating a different gap) must not interleave between them,
// or a stale sidecar could resurrect an already-closed gap after restart.
func (s *usageStore) openAccountingGap(at time.Time, cause error) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	s.analyticsReliabilityMu.Lock()
	defer s.analyticsReliabilityMu.Unlock()
	if s.analyticsGap == nil {
		s.analyticsGap = &AccountingGap{StartedAt: at.UTC(), Reason: fmt.Sprintf("durable usage write failed: %v", cause)}
	}
	s.persistActiveAccountingGapSidecarLocked(s.analyticsGap)
}

// accountingGapCloseHook, when non-nil, runs inside closeAccountingGap while
// analyticsReliabilityMu is held, after the closed gap has been durably
// persisted and before it is cleared. Tests use it to freeze the close
// mid-flight; it must not call back into the store (the lock is held).
var accountingGapCloseHook func()

func (s *usageStore) closeAccountingGap(at time.Time) {
	// The persist, the in-memory clear, and the sidecar removal run under ONE
	// lock hold. Releasing the lock between them let a failure arriving in
	// that window see the still-uncleared old gap, skip opening its own, and
	// then have its sidecar removed by the finishing close — silently losing
	// the new accounting gap.
	s.analyticsReliabilityMu.Lock()
	defer s.analyticsReliabilityMu.Unlock()
	if s.analyticsGap == nil {
		return
	}
	gap := *s.analyticsGap
	ended := at.UTC()
	if ended.Before(gap.StartedAt) {
		ended = time.Now().UTC()
	}
	gap.EndedAt = &ended
	if s.persistAccountingGap(gap) == nil {
		if accountingGapCloseHook != nil {
			accountingGapCloseHook()
		}
		if s.analyticsGap != nil && s.analyticsGap.StartedAt.Equal(gap.StartedAt) {
			s.analyticsGap = nil
		}
		_ = os.Remove(s.analyticsGapPath)
	}
}

func (s *usageStore) accountingGaps() ([]AccountingGap, *AccountingGap, error) {
	gaps := make([]AccountingGap, 0)
	err := s.db.View(func(tx *bbolt.Tx) error {
		gaps = s.loadAccountingGaps(tx)
		return nil
	})
	s.analyticsReliabilityMu.Lock()
	var active *AccountingGap
	if s.analyticsGap != nil {
		copy := *s.analyticsGap
		active = &copy
	}
	s.analyticsReliabilityMu.Unlock()
	return gaps, active, err
}

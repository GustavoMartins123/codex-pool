package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// CircuitState represents the operational state of a circuit breaker.
type CircuitState string

const (
	StateClosed   CircuitState = "CLOSED"
	StateOpen     CircuitState = "OPEN"
	StateHalfOpen CircuitState = "HALF_OPEN"

	maxCircuitEntries = 8192
)

var (
	ErrCircuitOpen            = errors.New("circuit breaker is OPEN")
	ErrCircuitHalfOpenProbing = errors.New("circuit breaker is HALF_OPEN and probe is already in flight")
)

type circuitEntry struct {
	mu                   sync.Mutex
	key                  string
	level                string
	state                CircuitState
	consecutiveFailures  int
	consecutiveSuccesses int
	cooldownUntil        time.Time
	lastFailure          time.Time
	lastSuccess          time.Time
	probeStartedAt       time.Time
	probesInFlight       int
	failureThreshold     int
	successThreshold     int
	cooldownDuration     time.Duration
	maxProbes            int
}

func (e *circuitEntry) expireStaleProbeLocked(now time.Time) {
	if e.state != StateHalfOpen || e.probesInFlight <= 0 || e.probeStartedAt.IsZero() {
		return
	}
	lease := e.cooldownDuration
	if lease < 5*time.Second {
		lease = 5 * time.Second
	}
	if now.Sub(e.probeStartedAt) > lease {
		e.probesInFlight = 0
		e.probeStartedAt = time.Time{}
	}
}

// CanAllow performs a non-mutating check of whether a request could be admitted
// without consuming a HALF_OPEN probe slot during candidate evaluation.
func (e *circuitEntry) CanAllow() (bool, error) {
	if e == nil {
		return true, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()
	switch e.state {
	case StateClosed:
		return true, nil

	case StateOpen:
		if now.After(e.cooldownUntil) {
			return true, nil
		}
		return false, ErrCircuitOpen

	case StateHalfOpen:
		e.expireStaleProbeLocked(now)
		if e.probesInFlight < e.maxProbes {
			return true, nil
		}
		return false, ErrCircuitHalfOpenProbing

	default:
		return true, nil
	}
}

func (e *circuitEntry) Allow() (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()
	switch e.state {
	case StateClosed:
		return true, nil

	case StateOpen:
		if now.After(e.cooldownUntil) {
			// Transition to HALF_OPEN and permit 1 probe request
			e.state = StateHalfOpen
			e.probesInFlight = 1
			e.probeStartedAt = now
			return true, nil
		}
		return false, ErrCircuitOpen

	case StateHalfOpen:
		e.expireStaleProbeLocked(now)
		if e.probesInFlight < e.maxProbes {
			e.probesInFlight++
			e.probeStartedAt = now
			return true, nil
		}
		return false, ErrCircuitHalfOpenProbing

	default:
		e.state = StateClosed
		return true, nil
	}
}

func (e *circuitEntry) ReleaseProbe() {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state == StateHalfOpen && e.probesInFlight > 0 {
		e.probesInFlight--
		if e.probesInFlight == 0 {
			e.probeStartedAt = time.Time{}
		}
	}
}

func (e *circuitEntry) RecordSuccess() {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	e.lastSuccess = time.Now()
	if e.state == StateHalfOpen {
		if e.probesInFlight > 0 {
			e.probesInFlight--
		}
		if e.probesInFlight == 0 {
			e.probeStartedAt = time.Time{}
		}
		e.consecutiveSuccesses++
		if e.consecutiveSuccesses >= e.successThreshold {
			e.state = StateClosed
			e.consecutiveFailures = 0
			e.consecutiveSuccesses = 0
		}
	} else if e.state == StateClosed {
		e.consecutiveFailures = 0
	}
}

func (e *circuitEntry) RecordFailure(errClass ErrorClass) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	if !shouldTripCircuit(errClass) {
		if e.state == StateHalfOpen && e.probesInFlight > 0 {
			e.probesInFlight--
			if e.probesInFlight == 0 {
				e.probeStartedAt = time.Time{}
			}
		}
		return
	}

	now := time.Now()
	e.lastFailure = now

	if e.state == StateHalfOpen {
		// Probe failed -> trip back to OPEN with doubled cooldown
		e.state = StateOpen
		if e.probesInFlight > 0 {
			e.probesInFlight--
		}
		e.probeStartedAt = time.Time{}
		e.consecutiveFailures++
		e.consecutiveSuccesses = 0
		e.cooldownUntil = now.Add(e.cooldownDuration * 2)
		return
	}

	if e.state == StateClosed {
		e.consecutiveFailures++
		if e.consecutiveFailures >= e.failureThreshold {
			e.state = StateOpen
			e.cooldownUntil = now.Add(e.cooldownDuration)
		}
	}
}

func (e *circuitEntry) StateInfo() CircuitStateInfo {
	e.mu.Lock()
	defer e.mu.Unlock()

	cooldownSec := float64(0)
	if e.state == StateOpen && e.cooldownUntil.After(time.Now()) {
		cooldownSec = time.Until(e.cooldownUntil).Seconds()
	}

	var lastFail *time.Time
	if !e.lastFailure.IsZero() {
		t := e.lastFailure
		lastFail = &t
	}
	var lastSucc *time.Time
	if !e.lastSuccess.IsZero() {
		t := e.lastSuccess
		lastSucc = &t
	}

	return CircuitStateInfo{
		Key:                  e.key,
		Level:                e.level,
		State:                e.state,
		ConsecutiveFailures:  e.consecutiveFailures,
		ConsecutiveSuccesses: e.consecutiveSuccesses,
		CooldownRemainingSec: cooldownSec,
		LastFailure:          lastFail,
		LastSuccess:          lastSucc,
	}
}

func shouldTripCircuit(errClass ErrorClass) bool {
	switch errClass {
	case ErrorClassRateLimit, ErrorClassTransient, ErrorClassPayment,
		ErrorClassNotFound, ErrorClassAuth:
		return true
	default:
		return false
	}
}

// CircuitStateInfo represents an external read-only view of a breaker's state.
type CircuitStateInfo struct {
	Key                  string       `json:"key"`
	Level                string       `json:"level"`
	State                CircuitState `json:"state"`
	ConsecutiveFailures  int          `json:"consecutive_failures"`
	ConsecutiveSuccesses int          `json:"consecutive_successes"`
	CooldownRemainingSec float64      `json:"cooldown_remaining_sec"`
	LastFailure          *time.Time   `json:"last_failure,omitempty"`
	LastSuccess          *time.Time   `json:"last_success,omitempty"`
}

// CircuitBreakerManager coordinates circuit breakers across 4 levels:
// provider, account, account+model, and account+capability.
type CircuitBreakerManager struct {
	mu      sync.RWMutex
	entries map[string]*circuitEntry
}

func newCircuitBreakerManager() *CircuitBreakerManager {
	return &CircuitBreakerManager{
		entries: make(map[string]*circuitEntry),
	}
}

func (m *CircuitBreakerManager) get(key string) *circuitEntry {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	entry := m.entries[key]
	m.mu.RUnlock()
	return entry
}

func (m *CircuitBreakerManager) getOrCreate(key, level string, failThreshold, succThreshold int, cooldown time.Duration) *circuitEntry {
	m.mu.RLock()
	if entry, ok := m.entries[key]; ok {
		m.mu.RUnlock()
		return entry
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	if entry, ok := m.entries[key]; ok {
		return entry
	}

	if len(m.entries) >= maxCircuitEntries {
		m.evictIdleEntriesLocked()
	}

	entry := &circuitEntry{
		key:              key,
		level:            level,
		state:            StateClosed,
		failureThreshold: failThreshold,
		successThreshold: succThreshold,
		cooldownDuration: cooldown,
		maxProbes:        1,
	}
	m.entries[key] = entry
	return entry
}

func (m *CircuitBreakerManager) evictIdleEntriesLocked() {
	now := time.Now()
	for k, e := range m.entries {
		e.mu.Lock()
		idleClosed := e.state == StateClosed && e.consecutiveFailures == 0
		expiredOpen := e.state == StateOpen && !e.cooldownUntil.IsZero() && now.Sub(e.cooldownUntil) > 10*time.Minute
		e.mu.Unlock()
		if idleClosed || expiredOpen {
			delete(m.entries, k)
		}
	}
}

// Level-specific keys
func providerKey(provider string) string {
	return "p:" + strings.ToLower(strings.TrimSpace(provider))
}

func accountKey(accountID string) string {
	return "a:" + strings.TrimSpace(accountID)
}

func accountModelKey(accountID, model string) string {
	return "am:" + strings.TrimSpace(accountID) + ":" + strings.ToLower(strings.TrimSpace(model))
}

func accountCapabilityKey(accountID, capability string) string {
	return "ac:" + strings.TrimSpace(accountID) + ":" + strings.ToLower(strings.TrimSpace(capability))
}

// CanAllowProvider checks provider-level circuit without consuming a probe slot.
func (m *CircuitBreakerManager) CanAllowProvider(provider string) (bool, error) {
	if m == nil || provider == "" {
		return true, nil
	}
	return m.get(providerKey(provider)).CanAllow()
}

// CanAllowAccount checks account-level circuit without consuming a probe slot.
func (m *CircuitBreakerManager) CanAllowAccount(accountID string) (bool, error) {
	if m == nil || accountID == "" {
		return true, nil
	}
	return m.get(accountKey(accountID)).CanAllow()
}

// CanAllowAccountModel checks model-specific circuit without consuming a probe slot.
func (m *CircuitBreakerManager) CanAllowAccountModel(accountID, model string) (bool, error) {
	if m == nil || accountID == "" || model == "" {
		return true, nil
	}
	return m.get(accountModelKey(accountID, model)).CanAllow()
}

// CanAllowAccountCapability checks capability circuit without consuming a probe slot.
func (m *CircuitBreakerManager) CanAllowAccountCapability(accountID, capability string) (bool, error) {
	if m == nil || accountID == "" || capability == "" {
		return true, nil
	}
	return m.get(accountCapabilityKey(accountID, capability)).CanAllow()
}

// CanAllowTarget checks all 4 levels without mutating state or consuming probe slots.
func (m *CircuitBreakerManager) CanAllowTarget(provider, accountID, model string, capabilities []string) (bool, string, error) {
	if m == nil {
		return true, "", nil
	}
	if provider != "" {
		if ok, err := m.CanAllowProvider(provider); !ok {
			return false, fmt.Sprintf("provider_%s_circuit_%v", provider, err), err
		}
	}
	if accountID != "" {
		if ok, err := m.CanAllowAccount(accountID); !ok {
			return false, fmt.Sprintf("account_%s_circuit_%v", accountID, err), err
		}
	}
	if accountID != "" && model != "" {
		if ok, err := m.CanAllowAccountModel(accountID, model); !ok {
			return false, fmt.Sprintf("account_%s_model_%s_circuit_%v", accountID, model, err), err
		}
	}
	if accountID != "" {
		for _, cap := range capabilities {
			if ok, err := m.CanAllowAccountCapability(accountID, cap); !ok {
				return false, fmt.Sprintf("account_%s_capability_%s_circuit_%v", accountID, cap, err), err
			}
		}
	}
	return true, "", nil
}

// AllowProvider checks provider-level circuit.
func (m *CircuitBreakerManager) AllowProvider(provider string) (bool, error) {
	if m == nil || provider == "" {
		return true, nil
	}
	entry := m.get(providerKey(provider))
	if entry == nil {
		return true, nil
	}
	return entry.Allow()
}

// AllowAccount checks account-level circuit.
func (m *CircuitBreakerManager) AllowAccount(accountID string) (bool, error) {
	if m == nil || accountID == "" {
		return true, nil
	}
	entry := m.get(accountKey(accountID))
	if entry == nil {
		return true, nil
	}
	return entry.Allow()
}

// AllowAccountModel checks model-specific circuit for a given account.
func (m *CircuitBreakerManager) AllowAccountModel(accountID, model string) (bool, error) {
	if m == nil || accountID == "" || model == "" {
		return true, nil
	}
	entry := m.get(accountModelKey(accountID, model))
	if entry == nil {
		return true, nil
	}
	return entry.Allow()
}

// AllowAccountCapability checks capability circuit for a given account.
func (m *CircuitBreakerManager) AllowAccountCapability(accountID, capability string) (bool, error) {
	if m == nil || accountID == "" || capability == "" {
		return true, nil
	}
	entry := m.get(accountCapabilityKey(accountID, capability))
	if entry == nil {
		return true, nil
	}
	return entry.Allow()
}

// AllowTarget checks all 4 levels before dispatching a request.
func (m *CircuitBreakerManager) AllowTarget(provider, accountID, model string, capabilities []string) (bool, string, error) {
	if m == nil {
		return true, "", nil
	}

	// Pre-validate all levels before claiming any HALF_OPEN probe slots.
	if ok, reason, err := m.CanAllowTarget(provider, accountID, model, capabilities); !ok {
		return false, reason, err
	}

	var acquired []*circuitEntry
	rollback := func() {
		for _, entry := range acquired {
			entry.ReleaseProbe()
		}
	}

	// 1. Provider level
	if provider != "" {
		if entry := m.get(providerKey(provider)); entry != nil {
			if ok, err := entry.Allow(); !ok {
				return false, fmt.Sprintf("provider_%s_circuit_%v", provider, err), err
			}
			acquired = append(acquired, entry)
		}
	}

	// 2. Account level
	if accountID != "" {
		if entry := m.get(accountKey(accountID)); entry != nil {
			if ok, err := entry.Allow(); !ok {
				rollback()
				return false, fmt.Sprintf("account_%s_circuit_%v", accountID, err), err
			}
			acquired = append(acquired, entry)
		}
	}

	// 3. Account + Model level
	if accountID != "" && model != "" {
		if entry := m.get(accountModelKey(accountID, model)); entry != nil {
			if ok, err := entry.Allow(); !ok {
				rollback()
				return false, fmt.Sprintf("account_%s_model_%s_circuit_%v", accountID, model, err), err
			}
			acquired = append(acquired, entry)
		}
	}

	// 4. Account + Capability level
	if accountID != "" {
		for _, cap := range capabilities {
			if entry := m.get(accountCapabilityKey(accountID, cap)); entry != nil {
				if ok, err := entry.Allow(); !ok {
					rollback()
					return false, fmt.Sprintf("account_%s_capability_%s_circuit_%v", accountID, cap, err), err
				}
				acquired = append(acquired, entry)
			}
		}
	}

	return true, "", nil
}

// RecordSuccess reports a successful request to reset/close breakers.
func (m *CircuitBreakerManager) RecordSuccess(provider, accountID, model string, capabilities []string) {
	if m == nil {
		return
	}
	if provider != "" {
		m.get(providerKey(provider)).RecordSuccess()
	}
	if accountID != "" {
		m.get(accountKey(accountID)).RecordSuccess()
	}
	if accountID != "" && model != "" {
		m.get(accountModelKey(accountID, model)).RecordSuccess()
	}
	if accountID != "" {
		for _, cap := range capabilities {
			m.get(accountCapabilityKey(accountID, cap)).RecordSuccess()
		}
	}
}

// RecordFailure reports an error to increment failure counters or trip breakers.
func (m *CircuitBreakerManager) RecordFailure(provider, accountID, model string, capabilities []string, errClass ErrorClass) {
	if m == nil {
		return
	}
	if !shouldTripCircuit(errClass) {
		if provider != "" {
			m.get(providerKey(provider)).RecordFailure(errClass)
		}
		if accountID != "" {
			m.get(accountKey(accountID)).RecordFailure(errClass)
		}
		if accountID != "" && model != "" && len(capabilities) == 0 {
			m.get(accountModelKey(accountID, model)).RecordFailure(errClass)
		}
		if accountID != "" {
			for _, cap := range capabilities {
				m.get(accountCapabilityKey(accountID, cap)).RecordFailure(errClass)
			}
		}
		return
	}
	if provider != "" {
		m.getOrCreate(providerKey(provider), "provider", 5, 1, 30*time.Second).RecordFailure(errClass)
	}
	if accountID != "" {
		m.getOrCreate(accountKey(accountID), "account", 3, 1, 30*time.Second).RecordFailure(errClass)
	}
	if accountID != "" && model != "" && len(capabilities) == 0 {
		m.getOrCreate(accountModelKey(accountID, model), "account_model", 2, 1, 45*time.Second).RecordFailure(errClass)
	}
	if accountID != "" {
		for _, cap := range capabilities {
			m.getOrCreate(accountCapabilityKey(accountID, cap), "account_capability", 2, 1, 60*time.Second).RecordFailure(errClass)
		}
	}
}

// Snapshot returns a copy of all tracked circuit states.
func (m *CircuitBreakerManager) Snapshot() map[string]CircuitStateInfo {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make(map[string]CircuitStateInfo, len(m.entries))
	for k, e := range m.entries {
		out[k] = e.StateInfo()
	}
	return out
}

// State returns the state of a specific circuit key, defaulting to CLOSED.
func (m *CircuitBreakerManager) State(key string) CircuitState {
	if m == nil {
		return StateClosed
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if e, ok := m.entries[key]; ok {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.state == StateOpen && time.Now().After(e.cooldownUntil) {
			return StateHalfOpen
		}
		return e.state
	}
	return StateClosed
}
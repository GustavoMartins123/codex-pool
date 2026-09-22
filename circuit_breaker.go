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
	probesInFlight       int
	failureThreshold     int
	successThreshold     int
	cooldownDuration     time.Duration
	maxProbes            int
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
			return true, nil
		}
		return false, ErrCircuitOpen

	case StateHalfOpen:
		if e.probesInFlight < e.maxProbes {
			e.probesInFlight++
			return true, nil
		}
		return false, ErrCircuitHalfOpenProbing

	default:
		e.state = StateClosed
		return true, nil
	}
}

func (e *circuitEntry) RecordSuccess() {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.lastSuccess = time.Now()
	if e.state == StateHalfOpen {
		if e.probesInFlight > 0 {
			e.probesInFlight--
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
	if !shouldTripCircuit(errClass) {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()
	e.lastFailure = now

	if e.state == StateHalfOpen {
		// Probe failed -> trip back to OPEN with doubled cooldown
		e.state = StateOpen
		if e.probesInFlight > 0 {
			e.probesInFlight--
		}
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

func (m *CircuitBreakerManager) getOrCreate(key, level string, failThreshold, succThreshold int, cooldown time.Duration) *circuitEntry {
	m.mu.Lock()
	defer m.mu.Unlock()

	if entry, ok := m.entries[key]; ok {
		return entry
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

// AllowProvider checks provider-level circuit.
func (m *CircuitBreakerManager) AllowProvider(provider string) (bool, error) {
	if m == nil || provider == "" {
		return true, nil
	}
	entry := m.getOrCreate(providerKey(provider), "provider", 5, 1, 30*time.Second)
	return entry.Allow()
}

// AllowAccount checks account-level circuit.
func (m *CircuitBreakerManager) AllowAccount(accountID string) (bool, error) {
	if m == nil || accountID == "" {
		return true, nil
	}
	entry := m.getOrCreate(accountKey(accountID), "account", 3, 1, 30*time.Second)
	return entry.Allow()
}

// AllowAccountModel checks model-specific circuit for a given account.
func (m *CircuitBreakerManager) AllowAccountModel(accountID, model string) (bool, error) {
	if m == nil || accountID == "" || model == "" {
		return true, nil
	}
	entry := m.getOrCreate(accountModelKey(accountID, model), "account_model", 2, 1, 45*time.Second)
	return entry.Allow()
}

// AllowAccountCapability checks capability circuit for a given account.
func (m *CircuitBreakerManager) AllowAccountCapability(accountID, capability string) (bool, error) {
	if m == nil || accountID == "" || capability == "" {
		return true, nil
	}
	entry := m.getOrCreate(accountCapabilityKey(accountID, capability), "account_capability", 2, 1, 60*time.Second)
	return entry.Allow()
}

// AllowTarget checks all 4 levels before dispatching a request.
func (m *CircuitBreakerManager) AllowTarget(provider, accountID, model string, capabilities []string) (bool, string, error) {
	if m == nil {
		return true, "", nil
	}

	// 1. Provider level
	if provider != "" {
		if ok, err := m.AllowProvider(provider); !ok {
			return false, fmt.Sprintf("provider_%s_circuit_%v", provider, err), err
		}
	}

	// 2. Account level
	if accountID != "" {
		if ok, err := m.AllowAccount(accountID); !ok {
			return false, fmt.Sprintf("account_%s_circuit_%v", accountID, err), err
		}
	}

	// 3. Account + Model level
	if accountID != "" && model != "" {
		if ok, err := m.AllowAccountModel(accountID, model); !ok {
			return false, fmt.Sprintf("account_%s_model_%s_circuit_%v", accountID, model, err), err
		}
	}

	// 4. Account + Capability level
	if accountID != "" {
		for _, cap := range capabilities {
			if ok, err := m.AllowAccountCapability(accountID, cap); !ok {
				return false, fmt.Sprintf("account_%s_capability_%s_circuit_%v", accountID, cap, err), err
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
		m.getOrCreate(providerKey(provider), "provider", 5, 1, 30*time.Second).RecordSuccess()
	}
	if accountID != "" {
		m.getOrCreate(accountKey(accountID), "account", 3, 1, 30*time.Second).RecordSuccess()
	}
	if accountID != "" && model != "" {
		m.getOrCreate(accountModelKey(accountID, model), "account_model", 2, 1, 45*time.Second).RecordSuccess()
	}
	if accountID != "" {
		for _, cap := range capabilities {
			m.getOrCreate(accountCapabilityKey(accountID, cap), "account_capability", 2, 1, 60*time.Second).RecordSuccess()
		}
	}
}

// RecordFailure reports an error to increment failure counters or trip breakers.
func (m *CircuitBreakerManager) RecordFailure(provider, accountID, model string, capabilities []string, errClass ErrorClass) {
	if m == nil {
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

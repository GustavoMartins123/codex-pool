package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// placeholderAccountMarker tags an account file that holds no credential. The
// accounts import writes it so an operator only signs in to each account on a
// new pool instead of adding all of them again. The provider skips the file
// for lack of credentials, and loadPool then builds the waiting account from
// it. The marker stops mattering as soon as a sign-in stores credentials.
const placeholderAccountMarker = "placeholder"

// placeholderAccountAuthError explains a waiting account on operator surfaces.
const placeholderAccountAuthError = "imported without credentials: sign in to activate this account"

// loadPlaceholderAccount returns the waiting account described by a file the
// provider skipped, or nil when the file is not marked as a placeholder.
func loadPlaceholderAccount(name, path string, accountType AccountType, data []byte) *Account {
	var root struct {
		Placeholder bool   `json:"placeholder"`
		AuthError   string `json:"auth_error"`
		Email       string `json:"email"`
		PlanType    string `json:"plan_type"`
	}
	if json.Unmarshal(data, &root) != nil || !root.Placeholder {
		return nil
	}
	reason := strings.TrimSpace(root.AuthError)
	if reason == "" {
		reason = placeholderAccountAuthError
	}
	return &Account{
		Type:        accountType,
		ID:          strings.TrimSuffix(name, filepath.Ext(name)),
		File:        path,
		Email:       strings.TrimSpace(root.Email),
		PlanType:    strings.TrimSpace(root.PlanType),
		NeedsLogin:  true,
		AuthError:   reason,
		placeholder: true,
	}
}

// isPlaceholderAccount reports whether the account was imported without
// credentials and still waits for its first sign-in.
func isPlaceholderAccount(a *Account) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return isPlaceholderAccountLocked(a)
}

func isPlaceholderAccountLocked(a *Account) bool {
	return a.placeholder
}

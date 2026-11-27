package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

const transferTestSecret = "accounts-export-test-secret-0123456789abcdef"

func transferTestHandler(t *testing.T) *proxyHandler {
	t.Helper()
	h := placeholderTestHandler(t)
	h.passportConfig = passportConfig{jwtSecret: transferTestSecret}
	return h
}

func registerTestAccount(t *testing.T, h *proxyHandler, acc *Account, owner string, operatorManaged bool) {
	t.Helper()
	err := h.passport.db.Update(func(tx *bbolt.Tx) error {
		ref, err := filepath.Rel(h.cfg.poolDir, acc.File)
		if err != nil {
			return err
		}
		resource := &accountResource{Version: 2, ID: acc.ID, Provider: acc.Type, OwnerID: owner, AddedBy: owner, OperatorManaged: operatorManaged, SecretRef: ref, Revision: 1, Status: "active"}
		return putJSON(tx.Bucket([]byte(bucketAccountResources)), resourceKey(acc.Type, acc.ID), resource)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// seededTransferHandler is a pool with every kind of account an export must
// tell apart. Each credential carries a recognizable secret value.
func seededTransferHandler(t *testing.T) *proxyHandler {
	t.Helper()
	h := transferTestHandler(t)
	writeTestAccountFile(t, h, "codex/codex_live.json", map[string]any{
		"tokens":     map[string]any{"id_token": codexTestIDToken(t, testCodexWorkspace, "user-a", "a@example.com"), "access_token": "codex-access-secret", "refresh_token": "codex-refresh-secret"},
		"added_at":   "2026-10-01T00:00:00Z",
		"auth_error": "stale-reason",
	})
	writeTestAccountFile(t, h, "codex/codex_dead.json", map[string]any{
		"tokens": map[string]any{"access_token": "dead-access-secret", "refresh_token": "dead-refresh-secret"},
		"dead":   true,
	})
	writeTestAccountFile(t, h, "codex/codex_private.json", map[string]any{
		"tokens":  map[string]any{"access_token": "private-access-secret", "refresh_token": "private-refresh-secret"},
		"cookies": map[string]any{"__cf_bm": "cookie-secret"},
	})
	writeTestAccountFile(t, h, "codex/codex_theirs.json", map[string]any{
		"tokens": map[string]any{"access_token": "theirs-access-secret", "refresh_token": "theirs-refresh-secret"},
	})
	writeTestAccountFile(t, h, "codex/codex_waiting.json", map[string]any{"placeholder": true, "email": "waiting@example.com"})
	writeTestAccountFile(t, h, "antigravity/antigravity_person.json", map[string]any{
		"access_token": "antigravity-access-secret", "refresh_token": "antigravity-refresh-secret", "project_id": "project-secret",
		"email": "person@example.com", "plan_type": "pro", "added_at": "2026-10-02T00:00:00Z",
	})
	writeTestAccountFile(t, h, "kimi/kimi_key.json", map[string]any{"api_key": "kimi-key-secret"})
	addTestPassportOperator(t, h.passport, "another-operator")
	if err := h.reloadAccounts(); err != nil {
		t.Fatal(err)
	}
	registerTestAccount(t, h, testAccountByID(t, h, "codex_live"), "operator", true)
	registerTestAccount(t, h, testAccountByID(t, h, "codex_theirs"), "another-operator", false)
	registerTestAccount(t, h, testAccountByID(t, h, "codex_dead"), "operator", true)
	registerTestAccount(t, h, testAccountByID(t, h, "codex_private"), "member", false)
	registerTestAccount(t, h, testAccountByID(t, h, "codex_waiting"), "operator", true)
	registerTestAccount(t, h, testAccountByID(t, h, "antigravity_person"), "operator", false)
	registerTestAccount(t, h, testAccountByID(t, h, "kimi_key"), "operator", true)
	return h
}

func exportedAccountNames(accounts []exportedAccount) []string {
	names := make([]string, 0, len(accounts))
	for _, account := range accounts {
		names = append(names, account.Provider+"/"+account.ID)
	}
	return names
}

func testAuditActions(h *proxyHandler) []string {
	var actions []string
	h.passport.db.View(func(tx *bbolt.Tx) error {
		tx.Bucket([]byte(bucketPassportAudit)).ForEach(func(_, raw []byte) error {
			var entry AuditEntry
			if json.Unmarshal(raw, &entry) == nil {
				actions = append(actions, entry.Action)
			}
			return nil
		})
		return nil
	})
	return actions
}

func TestAccountsExportCarriesNoCredentials(t *testing.T) {
	h := seededTransferHandler(t)
	session, _ := addTestPassportOperator(t, h.passport, "operator")

	response := httptest.NewRecorder()
	h.ServeHTTP(response, passportContributionRequest(http.MethodGet, "/admin/accounts/export", session, "", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content type=%q", got)
	}
	if got := response.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") {
		t.Fatalf("disposition=%q", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache control=%q", got)
	}

	body := response.Body.String()
	for _, secret := range []string{
		"codex-access-secret", "codex-refresh-secret", "dead-access-secret", "dead-refresh-secret",
		"private-access-secret", "private-refresh-secret", "cookie-secret", "theirs-access-secret", "theirs-refresh-secret", "antigravity-access-secret",
		"antigravity-refresh-secret", "project-secret", "kimi-key-secret", "waiting@example.com",
		"stale-reason", "auth_error", "pool" + string(os.PathSeparator),
	} {
		if strings.Contains(body, secret) {
			t.Fatalf("the export carries %q:\n%s", secret, body)
		}
	}

	var file accountsExport
	if err := json.Unmarshal(response.Body.Bytes(), &file); err != nil {
		t.Fatal(err)
	}
	if err := verifyAccountsExport(file, transferTestSecret); err != nil {
		t.Fatalf("the export is not signed for this pool's secret: %v", err)
	}
	if got, want := exportedAccountNames(file.Accounts), []string{"antigravity/antigravity_person", "codex/codex_live"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("exported %v, want %v", got, want)
	}
	antigravity, codex := file.Accounts[0], file.Accounts[1]
	if antigravity.Email != "person@example.com" || antigravity.PlanType != "pro" || antigravity.AddedAt != "2026-10-02T00:00:00Z" || antigravity.OperatorManaged {
		t.Fatalf("antigravity entry=%+v", antigravity)
	}
	if codex.Email != "a@example.com" || codex.AddedAt != "2026-10-01T00:00:00Z" || !codex.OperatorManaged || codex.PlanType == "" {
		t.Fatalf("codex entry=%+v", codex)
	}
	if file.Format != accountsExportFormat || file.Version != accountsExportVersion || file.ExportedAt == "" {
		t.Fatalf("export header=%+v", file)
	}
	if exported, err := time.Parse(time.RFC3339, file.ExportedAt); err != nil || time.Since(exported) > time.Minute {
		t.Fatalf("exported_at=%q", file.ExportedAt)
	}
}

func TestAccountsExportRequiresAnOperator(t *testing.T) {
	h := seededTransferHandler(t)

	anonymous := httptest.NewRecorder()
	h.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/admin/accounts/export", nil))
	// Without a configured admin token the server refuses every admin request.
	if anonymous.Code != http.StatusForbidden || anonymous.Body.Len() == 0 {
		t.Fatalf("anonymous status=%d body=%q", anonymous.Code, anonymous.Body.String())
	}

	memberSession, _ := addTestPassportOperator(t, h.passport, "member")
	h.passport.mu.Lock()
	member := *h.passport.principals["member"]
	member.Kind = PrincipalMember
	h.passport.principals[member.ID] = &member
	h.passport.mu.Unlock()
	if err := h.passport.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket([]byte(bucketPrincipals)), member.ID, &member)
	}); err != nil {
		t.Fatal(err)
	}
	memberResponse := httptest.NewRecorder()
	h.ServeHTTP(memberResponse, passportContributionRequest(http.MethodGet, "/admin/accounts/export", memberSession, "", nil))
	if memberResponse.Code != http.StatusForbidden || strings.Contains(memberResponse.Body.String(), "person@example.com") {
		t.Fatalf("member status=%d body=%s", memberResponse.Code, memberResponse.Body.String())
	}

	h.cfg.adminToken = "break-glass-token"
	breakGlass := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin/accounts/export", nil)
	request.Header.Set("X-Admin-Token", "break-glass-token")
	h.ServeHTTP(breakGlass, request)
	if breakGlass.Code != http.StatusOK || !strings.Contains(breakGlass.Body.String(), `"signature"`) {
		t.Fatalf("break-glass status=%d body=%s", breakGlass.Code, breakGlass.Body.String())
	}
	// The break-glass credential stands in for the operator role: it also
	// exports an operator's private account, but never a member's.
	var breakGlassFile accountsExport
	if err := json.Unmarshal(breakGlass.Body.Bytes(), &breakGlassFile); err != nil {
		t.Fatal(err)
	}
	if got, want := exportedAccountNames(breakGlassFile.Accounts), []string{"antigravity/antigravity_person", "codex/codex_live", "codex/codex_theirs"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("break-glass exported %v, want %v", got, want)
	}
	if err := verifyAccountsExport(breakGlassFile, transferTestSecret); err != nil {
		t.Fatalf("the break-glass export is not signed: %v", err)
	}
	for _, secret := range []string{"private-refresh-secret", "theirs-refresh-secret", "waiting@example.com"} {
		if strings.Contains(breakGlass.Body.String(), secret) {
			t.Fatalf("the break-glass export carries %q", secret)
		}
	}

	h.passportConfig = passportConfig{}
	unsigned := httptest.NewRecorder()
	h.ServeHTTP(unsigned, passportContributionRequest(http.MethodGet, "/admin/accounts/export", sessionOf(t, h, "operator"), "", nil))
	if unsigned.Code != http.StatusServiceUnavailable {
		t.Fatalf("status without a signing secret=%d body=%s", unsigned.Code, unsigned.Body.String())
	}
}

func sessionOf(t *testing.T, h *proxyHandler, id string) string {
	t.Helper()
	session, _ := addTestPassportOperator(t, h.passport, id)
	return session
}

func TestAccountsExportSignature(t *testing.T) {
	file := accountsExport{Format: accountsExportFormat, Version: accountsExportVersion, ExportedAt: "2026-11-27T12:00:00Z", Accounts: []exportedAccount{
		{Provider: "codex", ID: "codex_a", Email: "a@example.com"},
		{Provider: "antigravity", ID: "antigravity-b", OperatorManaged: true},
	}}
	if err := signAccountsExport(&file, ""); !errors.Is(err, errExportSigningUnavailable) {
		t.Fatalf("signing without a secret: %v", err)
	}
	if err := signAccountsExport(&file, transferTestSecret); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(file.Signature)
	if err != nil || len(raw) != sha256Size {
		t.Fatalf("signature=%q raw=%d err=%v", file.Signature, len(raw), err)
	}
	if err := verifyAccountsExport(file, transferTestSecret); err != nil {
		t.Fatalf("a fresh export was refused: %v", err)
	}

	// Formatting is not signed, content is: the same list re-indented verifies.
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, mustExportJSON(t, file), "", "  "); err != nil {
		t.Fatal(err)
	}
	var decoded accountsExport
	if err := json.Unmarshal(pretty.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if err := verifyAccountsExport(decoded, transferTestSecret); err != nil {
		t.Fatalf("a re-indented export was refused: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*accountsExport)
	}{
		{"no signature", func(f *accountsExport) { f.Signature = "" }},
		{"signature of another secret", func(f *accountsExport) { f.Signature = signedWith(f, "another-secret-"+strings.Repeat("y", 32)) }},
		{"changed email", func(f *accountsExport) { f.Accounts[0].Email = "attacker@example.com" }},
		{"added account", func(f *accountsExport) {
			f.Accounts = append(f.Accounts, exportedAccount{Provider: "codex", ID: "codex_c"})
		}},
		{"removed account", func(f *accountsExport) { f.Accounts = f.Accounts[1:] }},
		{"changed sharing", func(f *accountsExport) { f.Accounts[0].OperatorManaged = true }},
		{"changed export time", func(f *accountsExport) { f.ExportedAt = "2026-11-27T13:00:00Z" }},
		{"changed version", func(f *accountsExport) { f.Version = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tampered := file
			tampered.Accounts = append([]exportedAccount(nil), file.Accounts...)
			tc.mutate(&tampered)
			if err := verifyAccountsExport(tampered, transferTestSecret); !errors.Is(err, errExportSignature) {
				t.Fatalf("a changed export was accepted: %v", err)
			}
		})
	}
	if err := verifyAccountsExport(file, "another-secret-"+strings.Repeat("y", 32)); !errors.Is(err, errExportSignature) {
		t.Fatalf("another pool's secret was accepted: %v", err)
	}
	if err := verifyAccountsExport(file, ""); !errors.Is(err, errExportSigningUnavailable) {
		t.Fatalf("verification without a secret: %v", err)
	}
}

const sha256Size = 32

func mustExportJSON(t *testing.T, file accountsExport) []byte {
	t.Helper()
	raw, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func signedWith(file *accountsExport, secret string) string {
	signed := *file
	if err := signAccountsExport(&signed, secret); err != nil {
		return ""
	}
	return signed.Signature
}

func TestAccountsImportCreatesWaitingAccounts(t *testing.T) {
	source := seededTransferHandler(t)
	exported := source.exportAccounts("operator")

	target := transferTestHandler(t)
	// codex_live already exists here with its own credential: it must be left alone.
	live := writeTestAccountFile(t, target, "codex/codex_live.json", map[string]any{"tokens": map[string]any{"access_token": "target-access-secret", "refresh_token": "target-refresh-secret"}})
	if err := target.reloadAccounts(); err != nil {
		t.Fatal(err)
	}
	registerTestAccount(t, target, testAccountByID(t, target, "codex_live"), "operator", true)

	result, err := target.importAccounts("operator", exported.Accounts)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(result.Imported, ","), "antigravity/antigravity_person"; got != want {
		t.Fatalf("imported %q, want %q", got, want)
	}
	if got, want := strings.Join(result.Skipped, ","), "codex/codex_live"; got != want || len(result.Invalid) != 0 {
		t.Fatalf("skipped %q want %q, invalid %v", got, want, result.Invalid)
	}

	// The existing account was not overwritten by the import.
	raw, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	if plaintext, err := target.cfg.vault.readAccountFile(live); err != nil || !strings.Contains(string(plaintext), "target-refresh-secret") {
		t.Fatalf("existing account changed: %v %s", err, plaintext)
	} else if strings.Contains(string(raw), "target-refresh-secret") {
		t.Fatal("the pool directory holds a plaintext credential")
	}

	waiting := filepath.Join(target.cfg.poolDir, "antigravity", "antigravity_person.json")
	onDisk, err := os.ReadFile(waiting)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), "person@example.com") || strings.Contains(string(onDisk), placeholderAccountMarker) {
		t.Fatalf("the waiting account file is not sealed by the vault: %s", onDisk)
	}
	info, err := os.Stat(waiting)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("waiting account mode=%v err=%v", info.Mode().Perm(), err)
	}
	plaintext, err := target.cfg.vault.readAccountFile(waiting)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(plaintext, &stored); err != nil {
		t.Fatal(err)
	}
	stored[placeholderAccountMarker] = true
	for _, key := range []string{"access_token", "refresh_token", "id_token", "tokens", "api_key", "cookies", "project_id"} {
		if _, ok := stored[key]; ok {
			t.Fatalf("the waiting account file carries %q", key)
		}
	}
	if stored["email"] != "person@example.com" || stored["plan_type"] != "pro" || stored["added_at"] != "2026-10-02T00:00:00Z" {
		t.Fatalf("waiting account settings: %v", stored)
	}

	if err := target.reloadAccounts(); err != nil {
		t.Fatal(err)
	}
	acc := testAccountByID(t, target, "antigravity_person")
	if !isPlaceholderAccount(acc) || !acc.NeedsLogin || acc.AccessToken != "" || acc.Email != "person@example.com" {
		t.Fatalf("waiting account: placeholder=%v needs_login=%v email=%q", isPlaceholderAccount(acc), acc.NeedsLogin, acc.Email)
	}
	if err := target.passport.authorizeAccount("operator", acc, "manage"); err != nil {
		t.Fatalf("the importing operator cannot manage the waiting account: %v", err)
	}
	if err := target.passport.authorizeAccount("operator", acc, "use"); err != nil {
		t.Fatalf("the owner cannot use its waiting account: %v", err)
	}

	again, err := target.importAccounts("operator", exported.Accounts)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Imported) != 0 || len(again.Skipped) != 2 {
		t.Fatalf("a repeated import changed the pool: %+v", again)
	}
	if err := target.reloadAccounts(); err != nil {
		t.Fatal(err)
	}
	if got := len(target.pool.allAccounts()); got != 2 {
		t.Fatalf("the pool holds %d accounts after importing twice", got)
	}
}

func TestAccountsImportKeepsOperatorManagedAccountsShared(t *testing.T) {
	h := transferTestHandler(t)
	addTestPassportOperator(t, h.passport, "another-operator")
	result, err := h.importAccounts("operator", []exportedAccount{{Provider: "codex", ID: "codex_shared", OperatorManaged: true}, {Provider: "codex", ID: "codex_private"}})
	if err != nil || len(result.Imported) != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if err := h.reloadAccounts(); err != nil {
		t.Fatal(err)
	}
	shared := testAccountByID(t, h, "codex_shared")
	private := testAccountByID(t, h, "codex_private")
	for _, actor := range []string{"operator", "another-operator"} {
		if err := h.passport.authorizeAccount(actor, shared, "manage"); err != nil {
			t.Fatalf("operator %s cannot manage the shared account: %v", actor, err)
		}
	}
	if err := h.passport.authorizeAccount("operator", private, "manage"); err != nil {
		t.Fatalf("the owner cannot manage its private account: %v", err)
	}
	if err := h.passport.authorizeAccount("another-operator", private, "manage"); err == nil {
		t.Fatal("another operator manages a private account")
	}
}

func TestAccountsImportRejectsUnusableEntries(t *testing.T) {
	h := transferTestHandler(t)
	long := strings.Repeat("a", 200)
	file := accountsExport{Format: accountsExportFormat, Version: accountsExportVersion, Accounts: []exportedAccount{
		{Provider: "kimi", ID: "kimi_key"},
		{Provider: "codex", ID: "../../outside"},
		{Provider: "codex", ID: ".."},
		{Provider: "codex", ID: ".hidden"},
		{Provider: "codex", ID: "codex ok"},
		{Provider: "codex", ID: "codex_ok", Email: strings.Repeat("e", 300)},
		{Provider: "codex", ID: long},
		{Provider: "codex", ID: "codex_ok"},
		{Provider: "codex", ID: "codex_twice"},
		{Provider: "codex", ID: "codex_twice"},
	}}
	result, err := h.importAccounts("operator", file.Accounts)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(result.Imported, ","), "codex/codex_ok,codex/codex_twice"; got != want {
		t.Fatalf("imported %q, want %q", got, want)
	}
	if got, want := strings.Join(result.Skipped, ","), "codex/codex_twice"; got != want {
		t.Fatalf("skipped %q, want %q", got, want)
	}
	wantInvalid := []string{"kimi/kimi_key", "codex/../../outside", "codex/..", "codex/.hidden", "codex/codex ok", "codex/codex_ok", "codex/" + long}
	if got := strings.Join(result.Invalid, ","); got != strings.Join(wantInvalid, ",") {
		t.Fatalf("invalid %v, want %v", result.Invalid, wantInvalid)
	}

	var written []string
	err = filepath.WalkDir(h.cfg.poolDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			written = append(written, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 2 || filepath.Base(written[0]) != "codex_ok.json" || filepath.Base(written[1]) != "codex_twice.json" {
		t.Fatalf("files written: %v", written)
	}
	if _, err := os.Stat(filepath.Join(h.cfg.poolDir, "outside")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an entry escaped the pool directory: %v", err)
	}
}

func TestAccountsImportHTTP(t *testing.T) {
	source := seededTransferHandler(t)
	exported := source.exportAccounts("operator")
	var body bytes.Buffer
	if err := json.Indent(&body, mustExportJSON(t, exported), "", "  "); err != nil {
		t.Fatal(err)
	}

	target := transferTestHandler(t)
	session, csrf := addTestPassportOperator(t, target.passport, "operator")

	withoutCSRF := httptest.NewRecorder()
	target.ServeHTTP(withoutCSRF, passportContributionRequest(http.MethodPost, "/admin/accounts/import", session, "", body.Bytes()))
	if withoutCSRF.Code != http.StatusForbidden || len(target.pool.allAccounts()) != 0 {
		t.Fatalf("status without CSRF=%d body=%s", withoutCSRF.Code, withoutCSRF.Body.String())
	}

	accepted := httptest.NewRecorder()
	target.ServeHTTP(accepted, passportContributionRequest(http.MethodPost, "/admin/accounts/import", session, csrf, body.Bytes()))
	if accepted.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	var result accountsImportResult
	if err := json.Unmarshal(accepted.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.Imported, ",") != "antigravity/antigravity_person,codex/codex_live" || len(result.Skipped) != 0 || len(result.Invalid) != 0 {
		t.Fatalf("result=%+v", result)
	}
	if !isPlaceholderAccount(testAccountByID(t, target, "codex_live")) {
		t.Fatal("the request did not make the waiting account visible")
	}
	if actions := testAuditActions(target); !containsAction(actions, "accounts.import") {
		t.Fatalf("audit actions=%v", actions)
	}

	tamper := func(mutate func(map[string]any)) []byte {
		t.Helper()
		var document map[string]any
		if err := json.Unmarshal(body.Bytes(), &document); err != nil {
			t.Fatal(err)
		}
		mutate(document)
		changed, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	for name, changed := range map[string][]byte{
		"changed account": tamper(func(document map[string]any) { document["accounts"].([]any)[0].(map[string]any)["id"] = "codex_evil" }),
		"no signature":    tamper(func(document map[string]any) { delete(document, "signature") }),
		"added account": tamper(func(document map[string]any) {
			document["accounts"] = append(document["accounts"].([]any), map[string]any{"provider": "codex", "id": "codex_evil"})
		}),
	} {
		refused := httptest.NewRecorder()
		target.ServeHTTP(refused, passportContributionRequest(http.MethodPost, "/admin/accounts/import", session, csrf, changed))
		if refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), "signature") {
			t.Fatalf("%s: status=%d body=%s", name, refused.Code, refused.Body.String())
		}
	}

	target.passportConfig = passportConfig{jwtSecret: "another-secret-" + strings.Repeat("z", 32)}
	otherSecret := httptest.NewRecorder()
	target.ServeHTTP(otherSecret, passportContributionRequest(http.MethodPost, "/admin/accounts/import", session, csrf, body.Bytes()))
	if otherSecret.Code != http.StatusBadRequest || !strings.Contains(otherSecret.Body.String(), "signature") {
		t.Fatalf("another pool's secret: status=%d body=%s", otherSecret.Code, otherSecret.Body.String())
	}

	for name, invalid := range map[string][]byte{
		"not an export":  []byte(`{"format":"something-else","version":1}`),
		"newer version":  []byte(`{"format":"` + accountsExportFormat + `","version":99}`),
		"not json":       []byte("accounts"),
		"truncated list": []byte(`{"format":"` + accountsExportFormat + `","version":1,"accounts":[{"provider":"codex"`),
	} {
		refused := httptest.NewRecorder()
		target.ServeHTTP(refused, passportContributionRequest(http.MethodPost, "/admin/accounts/import", session, csrf, invalid))
		if refused.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", name, refused.Code, refused.Body.String())
		}
	}

	tooMany, err := json.Marshal(accountsExport{Format: accountsExportFormat, Version: accountsExportVersion})
	if err != nil {
		t.Fatal(err)
	}
	oversized := httptest.NewRecorder()
	target.ServeHTTP(oversized, passportContributionRequest(http.MethodPost, "/admin/accounts/import", session, csrf, append(tooMany[:len(tooMany)-1], []byte(`,"accounts":[`+strings.Repeat(`{"provider":"codex","id":"x"},`, maxImportedAccounts)+`{"provider":"codex","id":"y"}]}`)...)))
	if oversized.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status=%d body=%s", oversized.Code, oversized.Body.String())
	}

	memberSession, _ := addTestPassportOperator(t, target.passport, "member")
	target.passport.mu.Lock()
	member := *target.passport.principals["member"]
	member.Kind = PrincipalMember
	target.passport.principals[member.ID] = &member
	target.passport.mu.Unlock()
	if err := target.passport.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket([]byte(bucketPrincipals)), member.ID, &member)
	}); err != nil {
		t.Fatal(err)
	}
	memberResponse := httptest.NewRecorder()
	target.ServeHTTP(memberResponse, passportContributionRequest(http.MethodPost, "/admin/accounts/import", memberSession, csrf, body.Bytes()))
	if memberResponse.Code != http.StatusForbidden {
		t.Fatalf("member status=%d body=%s", memberResponse.Code, memberResponse.Body.String())
	}
}

func containsAction(actions []string, action string) bool {
	for _, candidate := range actions {
		if candidate == action {
			return true
		}
	}
	return false
}

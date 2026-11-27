package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

func (h *proxyHandler) handleAccountsExport(w http.ResponseWriter, r *http.Request) {
	if h.passport == nil || h.pool == nil {
		respondJSONError(w, http.StatusServiceUnavailable, "account authority unavailable")
		return
	}
	file := h.exportAccounts(providerContributionActor(r))
	if file.Signature == "" {
		respondJSONError(w, http.StatusServiceUnavailable, errExportSigningUnavailable.Error())
		return
	}
	h.recordAdminAudit(r, "accounts.export", "", fmt.Sprintf("%d accounts", len(file.Accounts)))
	noStore(w)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="codex-pool-accounts.json"`)
	json.NewEncoder(w).Encode(file)
}

func (h *proxyHandler) handleAccountsImport(w http.ResponseWriter, r *http.Request) {
	if h.passport == nil || h.pool == nil || h.cfg == nil {
		respondJSONError(w, http.StatusServiceUnavailable, "account authority unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var file accountsExport
	if err := json.NewDecoder(r.Body).Decode(&file); err != nil {
		respondJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if file.Format != accountsExportFormat || file.Version != accountsExportVersion {
		respondJSONError(w, http.StatusBadRequest, "not a codex-pool accounts export")
		return
	}
	if len(file.Accounts) > maxImportedAccounts {
		respondJSONError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("an import holds at most %d accounts", maxImportedAccounts))
		return
	}
	if err := verifyAccountsExport(file, h.passportConfig.jwtSecret); err != nil {
		respondJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := h.importAccounts(providerContributionActor(r), file.Accounts)
	if err != nil {
		respondJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.reloadAccounts(); err != nil {
		respondJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.recordAdminAudit(r, "accounts.import", "", fmt.Sprintf("%d imported, %d skipped, %d invalid", len(result.Imported), len(result.Skipped), len(result.Invalid)))
	respondJSON(w, result)
}

package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	usageExportDefaultLimit = 50000
	usageExportMaxLimit     = 100000
)

var usageExportColumns = []string{
	"observed_at", "principal_id", "client_credential_id", "origin_id", "account_id", "account_type",
	"plan_type", "model", "input_tokens", "cache_read_tokens", "cache_creation_tokens", "output_tokens",
	"reasoning_tokens", "billable_tokens", "api_equivalent_cost_usd",
}

// GET /api/console/usage-export - operator exports the whole ledger or one principal.
func (h *proxyHandler) handleConsoleUsageExport(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	operator, _, ok := h.requireOperator(w, r)
	if !ok {
		return
	}
	principalID := strings.TrimSpace(r.URL.Query().Get("principal"))
	if principalID != "" && h.passport.principal(principalID) == nil {
		http.NotFound(w, r)
		return
	}
	h.serveUsageExport(w, r, principalID, operator.ID)
}

// GET /api/me/usage-export - members export only their own rows.
func (h *proxyHandler) handleMyUsageExport(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	principal, _ := h.passport.authenticate(r)
	if principal == nil {
		respondJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	h.serveUsageExport(w, r, principal.ID, principal.ID)
}

func usageExportLimit(r *http.Request) int {
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 || limit > usageExportMaxLimit {
		return usageExportDefaultLimit
	}
	return limit
}

func (h *proxyHandler) serveUsageExport(w http.ResponseWriter, r *http.Request, principalID, actorID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.duckAnalytics == nil {
		respondJSONError(w, http.StatusServiceUnavailable, "analytics unavailable")
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "json" {
		respondJSONError(w, http.StatusBadRequest, "format must be csv or json")
		return
	}
	hours := consoleUsageHours(r)
	limit := usageExportLimit(r)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rows := make([]UsageExportRow, 0)
	count, truncated, err := h.duckAnalytics.ExportUsage(ctx, UsageExportQuery{
		PrincipalID: principalID,
		Since:       time.Now().Add(-time.Duration(hours) * time.Hour),
		Limit:       limit,
	}, func(row UsageExportRow) error {
		rows = append(rows, row)
		return nil
	})
	if err != nil {
		log.Printf("usage export query failed: %v", err)
		respondJSONError(w, http.StatusInternalServerError, "analytics query failed")
		return
	}

	scope := "all"
	if principalID != "" {
		scope = exportFilenameScope(principalID)
	}
	filename := fmt.Sprintf("usage-export-%s-%s.%s", scope, time.Now().UTC().Format("20060102T150405Z"), format)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("X-Usage-Export-Rows", strconv.Itoa(count))
	w.Header().Set("X-Usage-Export-Truncated", strconv.FormatBool(truncated))

	if format == "json" {
		respondJSON(w, map[string]any{
			"rows":                 rows,
			"rows_count":           count,
			"truncated":            truncated,
			"hours":                hours,
			"limit":                limit,
			"excludes_passthrough": true,
		})
	} else {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		writer := csv.NewWriter(w)
		_ = writer.Write(usageExportColumns)
		for _, row := range rows {
			_ = writer.Write(usageExportCSVRecord(row))
		}
		writer.Flush()
	}

	if h.passport != nil {
		detail := fmt.Sprintf("rows=%d format=%s hours=%d truncated=%t", count, format, hours, truncated)
		if err := h.passport.recordAudit(actorID, "usage.exported", principalID, detail); err != nil {
			log.Printf("record usage.exported audit: %v", err)
		}
	}
}

func usageExportCSVRecord(row UsageExportRow) []string {
	return []string{
		row.ObservedAt.UTC().Format(time.RFC3339),
		csvSafeField(row.PrincipalID),
		csvSafeField(row.ClientCredentialID),
		csvSafeField(row.OriginID),
		csvSafeField(row.AccountID),
		csvSafeField(row.AccountType),
		csvSafeField(row.PlanType),
		csvSafeField(row.Model),
		strconv.FormatInt(row.InputTokens, 10),
		strconv.FormatInt(row.CacheReadTokens, 10),
		strconv.FormatInt(row.CacheCreationTokens, 10),
		strconv.FormatInt(row.OutputTokens, 10),
		strconv.FormatInt(row.ReasoningTokens, 10),
		strconv.FormatInt(row.BillableTokens, 10),
		strconv.FormatFloat(row.APIEquivalentCostUSD, 'f', 9, 64),
	}
}

// csvSafeField defuses spreadsheet formula injection: a cell opening with one
// of these characters is executed by Excel/Sheets instead of being displayed.
func csvSafeField(value string) string {
	if value == "" {
		return value
	}
	switch value[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + value
	}
	return value
}

func exportFilenameScope(principalID string) string {
	var safe strings.Builder
	for _, char := range principalID {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			safe.WriteRune(char)
		}
		if safe.Len() >= 64 {
			break
		}
	}
	if safe.Len() == 0 {
		return "principal"
	}
	return safe.String()
}
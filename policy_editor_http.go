package main

import (
	"net/http"
	"strings"
)

func (h *proxyHandler) handlePolicyEditor(w http.ResponseWriter, r *http.Request, principalID string, preview bool) {
	noStore(w)
	actor, session, ok := h.requireOperator(w, r)
	if !ok {
		return
	}
	q := policyEditorRequest{ClientID: r.URL.Query().Get("client_id")}
	if r.Method != http.MethodGet {
		if (preview && r.Method != http.MethodPost) || (!preview && r.Method != http.MethodPut) {
			http.Error(w, "method not allowed", 405)
			return
		}
		if !h.passportCSRF(r, session) {
			respondJSONError(w, 403, "invalid CSRF token")
			return
		}
		if decodeGovernanceJSON(w, r, &q) != nil || q.Policy == nil {
			respondJSONError(w, 400, "invalid policy request")
			return
		}
	} else if preview {
		http.Error(w, "method not allowed", 405)
		return
	}
	view, err := h.policyEditorView(principalID, &q)
	if err != nil {
		respondPolicyError(w, err)
		return
	}
	if r.Method == http.MethodPut {
		if err := h.passport.saveEditedPolicy(actor.ID, principalID, q); err != nil {
			respondPolicyError(w, err)
			return
		}
		view, err = h.policyEditorView(principalID, &policyEditorRequest{ClientID: q.ClientID})
		if err != nil {
			respondPolicyError(w, err)
			return
		}
	}
	respondJSON(w, view)
}

func policyEditorPath(path string) (string, bool, bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/api/console/policies/"), "/")
	if len(parts) == 1 && parts[0] != "" {
		return parts[0], false, true
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "preview" {
		return parts[0], true, true
	}
	return "", false, false
}

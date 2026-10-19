package main

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
)

func TestMemberQuotasBrowserE2E(t *testing.T) {
	if os.Getenv("POOL_BROWSER_E2E") != "1" {
		t.Skip("set POOL_BROWSER_E2E=1 and CHROME_PATH to run the real browser and backend")
	}
	if os.Getenv("CHROME_PATH") == "" {
		t.Fatal("CHROME_PATH is required")
	}
	h, id, session, csrf, operator, opCSRF := memberQuotaFixture(t)
	server := httptest.NewServer(h)
	defer server.Close()
	cmd := exec.Command("node", "web/member-quotas.e2e.mjs")
	cmd.Env = append(os.Environ(), "UI_TEST_ORIGIN="+server.URL, "E2E_MEMBER_ID="+id, "E2E_MEMBER_SESSION="+session, "E2E_MEMBER_CSRF="+csrf, "E2E_OPERATOR_SESSION="+operator, "E2E_OPERATOR_CSRF="+opCSRF)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("browser: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
}

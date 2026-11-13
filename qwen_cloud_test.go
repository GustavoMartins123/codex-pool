package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func qwenCloudEnvelope(payload string) string {
	return `{"code":"200","successResponse":true,"data":{"success":true,"DataV2":{"data":{"code":"SUCCESS","success":true,"data":` + payload + `}}}}`
}

func TestQwenCloudPersonalReadsPlanAllowanceAndFraction(t *testing.T) {
	for _, fraction := range []float64{0, .25, 1} {
		t.Run(fmt.Sprint(fraction), func(t *testing.T) {
			now := time.Now().UTC()
			end := now.Add(29 * 24 * time.Hour)
			calls := 0
			h := qwenTestHandler(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.Path == "/modelstudio/cli/generateAccessToken" {
					if req.URL.Host != "modelstudio.ap-southeast-1.aliyuncs.com" || req.Method != http.MethodPost || req.Header.Get("x-acs-action") != "GenerateCLIAccessToken" || !strings.HasPrefix(req.Header.Get("Authorization"), "ACS3-HMAC-SHA256 ") {
						t.Fatal("Cloud account was not authenticated through the official signed token exchange")
					}
					return qwenJSONResponse(200, `{"cliAccessToken":"synthetic-cloud-session"}`), nil
				}
				if req.URL.Host != "bailian-singapore-cs.alibabacloud.com" || req.URL.Path != "/cli/api.json" || req.Header.Get("Authorization") != "Bearer synthetic-cloud-session" || req.Header.Get("Cookie") != "" {
					t.Fatalf("wrong Cloud gateway request: %s", req.URL)
				}
				body, _ := io.ReadAll(req.Body)
				form, err := url.ParseQuery(string(body))
				if err != nil || form.Get("region") != "ap-southeast-1" || form.Get("sec_token") != "" {
					t.Fatal("invalid CLI gateway form")
				}
				var params struct {
					Api  string
					V    string
					Data map[string]json.RawMessage
				}
				if json.Unmarshal([]byte(form.Get("params")), &params) != nil || params.Api != req.URL.Query().Get("api") || params.Data["cornerstoneParam"] == nil {
					t.Fatal("missing gateway protocol parameters")
				}
				switch strings.TrimPrefix(params.Api, qwenCloudPersonalAPI) {
				case "subscription":
					if !strings.Contains(string(params.Data["queryInstanceInfoRequest"]), "sfm_tokenplansolo_public_intl") {
						t.Fatal("wrong Personal subscription product")
					}
					return qwenJSONResponse(200, qwenCloudEnvelope(fmt.Sprintf(`{"instanceCode":"synthetic-instance","specCode":"essential","startTime":%d,"endTime":%d,"status":"VALID"}`, now.Add(-24*time.Hour).UnixMilli(), end.UnixMilli()))), nil
				case "quota-config":
					return qwenJSONResponse(200, qwenCloudEnvelope(`{"essential":{"monthly":45000},"pro":{"monthly":200000}}`)), nil
				case "usage":
					return qwenJSONResponse(200, qwenCloudEnvelope(fmt.Sprintf(`{"per1MonthPercentage":%g,"per1MonthResetTime":%d}`, fraction, end.UnixMilli()))), nil
				default:
					t.Fatalf("unexpected Cloud operation: %s", params.Api)
					return nil, nil
				}
			}))
			plan := &QwenTokenPlanConfig{Mode: qwenPersonalCloud, AccessKeyID: "test-access-key-id", AccessKeySecret: "test-access-key-secret"}
			credits, err := h.queryQwenCredits(context.Background(), plan, now)
			if err != nil || calls != 4 {
				t.Fatalf("Cloud query failed: %v calls=%d", err, calls)
			}
			if credits.Plan != "essential" || credits.Total != 45000 || credits.Used != 45000*fraction || credits.Remaining != 45000*(1-fraction) || credits.Source != "alibaba-console" || qwenCreditUsage(credits, now).PrimaryUsedPercent != fraction {
				t.Fatalf("wrong automatic Cloud balance: %+v", credits)
			}
		})
	}
}

func TestQwenCloudFailsClosedOnUnknownPlanAndMissingUsage(t *testing.T) {
	for _, failure := range []string{"auth", "subscription", "quota-config", "usage", "fraction"} {
		t.Run(failure, func(t *testing.T) {
			now := time.Now().UTC()
			end := now.Add(24 * time.Hour).UnixMilli()
			h := qwenTestHandler(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/modelstudio/cli/generateAccessToken" {
					if failure == "auth" {
						return qwenJSONResponse(403, `{"Message":"synthetic upstream secret"}`), nil
					}
					return qwenJSONResponse(200, `{"cliAccessToken":"synthetic-cloud-session"}`), nil
				}
				switch strings.TrimPrefix(req.URL.Query().Get("api"), qwenCloudPersonalAPI) {
				case "subscription":
					if failure == "subscription" {
						return qwenJSONResponse(200, qwenCloudEnvelope(`{"status":"EXPIRED"}`)), nil
					}
					return qwenJSONResponse(200, qwenCloudEnvelope(fmt.Sprintf(`{"instanceCode":"instance","specCode":"essential","startTime":%d,"endTime":%d,"status":"VALID"}`, now.Add(-time.Hour).UnixMilli(), end))), nil
				case "quota-config":
					if failure == "quota-config" {
						return qwenJSONResponse(200, qwenCloudEnvelope(`{"pro":{"monthly":200000}}`)), nil
					}
					return qwenJSONResponse(200, qwenCloudEnvelope(`{"essential":{"monthly":45000}}`)), nil
				case "usage":
					if failure == "usage" {
						return qwenJSONResponse(200, qwenCloudEnvelope(`{}`)), nil
					}
					return qwenJSONResponse(200, qwenCloudEnvelope(fmt.Sprintf(`{"per1MonthPercentage":25,"per1MonthResetTime":%d}`, end))), nil
				}
				t.Fatal("unexpected endpoint")
				return nil, nil
			}))
			credits, err := h.queryQwenCredits(context.Background(), &QwenTokenPlanConfig{Mode: qwenPersonalCloud, AccessKeyID: "key", AccessKeySecret: "secret"}, now)
			if err == nil || credits != nil || strings.Contains(err.Error(), "synthetic upstream secret") {
				t.Fatalf("invalid Cloud response produced credits or exposed an upstream body: %+v %v", credits, err)
			}
		})
	}
}

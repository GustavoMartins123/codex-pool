package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	qwenCloudGateway     = "https://bailian-singapore-cs.alibabacloud.com/cli/api.json"
	qwenCloudPersonalAPI = "zeldaHttp.apikeyMgr./tokenplan/personal/api/v2/"
)

func (h *proxyHandler) qwenCloudAccessToken(ctx context.Context, plan *QwenTokenPlanConfig, now time.Time) (string, error) {
	if h.cfg.qwenPlanBase == nil {
		return "", errors.New("Alibaba Cloud management endpoint is not configured")
	}
	target := *h.cfg.qwenPlanBase
	target.Path = singleJoin(target.Path, "/modelstudio/cli/generateAccessToken")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), nil)
	if err != nil {
		return "", errors.New("could not create Alibaba Cloud account authentication request")
	}
	signQwenPlanRequest(req, plan, "GenerateCLIAccessToken", now, uuid.NewString())
	req.Header.Set("Accept", "application/json")
	response, err := h.transport.RoundTrip(req)
	if err != nil {
		return "", errors.New("Alibaba Cloud account authentication failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("Alibaba Cloud account authentication returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Token   string `json:"cliAccessToken"`
		Success *bool  `json:"Success"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload) != nil || payload.Success != nil && !*payload.Success || strings.TrimSpace(payload.Token) == "" {
		return "", errors.New("Alibaba Cloud did not issue a valid CLI access token")
	}
	return strings.TrimSpace(payload.Token), nil
}

func (h *proxyHandler) qwenCloudRequest(ctx context.Context, token, operation string, data map[string]any, output any) error {
	api := qwenCloudPersonalAPI + operation
	data["cornerstoneParam"] = map[string]any{"protocol": "V2", "console": "ONE_CONSOLE", "productCode": "p_efm", "switchUserType": 3, "consoleSite": "BAILIAN_ALIYUN"}
	params, err := json.Marshal(map[string]any{"Api": api, "V": "1.0", "Data": data})
	if err != nil {
		return errors.New("could not encode Alibaba Cloud subscription request")
	}
	query := url.Values{"action": {"IntlBroadScopeAspnGateway"}, "product": {"sfm_bailian"}, "api": {api}}
	form := url.Values{"params": {string(params)}, "region": {"ap-southeast-1"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, qwenCloudGateway+"?"+query.Encode(), strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("could not create Alibaba Cloud subscription request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := h.transport.RoundTrip(req)
	if err != nil {
		return errors.New("Alibaba Cloud subscription request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Alibaba Cloud subscription returned HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Code            string `json:"code"`
		SuccessResponse bool   `json:"successResponse"`
		Data            struct {
			Success bool `json:"success"`
			DataV2  struct {
				Data struct {
					Success bool            `json:"success"`
					Code    string          `json:"code"`
					Data    json.RawMessage `json:"data"`
				} `json:"data"`
			} `json:"DataV2"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope) != nil || envelope.Code != "200" || !envelope.SuccessResponse || !envelope.Data.Success || !envelope.Data.DataV2.Data.Success || envelope.Data.DataV2.Data.Code != "SUCCESS" {
		return errors.New("Alibaba Cloud rejected the subscription query; check Token Plan permissions and account access")
	}
	if json.Unmarshal(envelope.Data.DataV2.Data.Data, output) != nil {
		return errors.New("Alibaba Cloud returned an invalid subscription payload")
	}
	return nil
}

func (h *proxyHandler) queryQwenCloudPersonalCredits(ctx context.Context, plan *QwenTokenPlanConfig, now time.Time) (*QwenCreditSnapshot, error) {
	token, err := h.qwenCloudAccessToken(ctx, plan, now)
	if err != nil {
		return nil, err
	}
	var subscription struct {
		InstanceCode string `json:"instanceCode"`
		SpecCode     string `json:"specCode"`
		StartTime    int64  `json:"startTime"`
		EndTime      int64  `json:"endTime"`
		Status       string `json:"status"`
	}
	if err := h.qwenCloudRequest(ctx, token, "subscription", map[string]any{"queryInstanceInfoRequest": map[string]string{"commodityCode": "sfm_tokenplansolo_public_intl"}}, &subscription); err != nil {
		return nil, err
	}
	start, end := time.UnixMilli(subscription.StartTime), time.UnixMilli(subscription.EndTime)
	if subscription.Status != "VALID" || subscription.InstanceCode == "" || subscription.SpecCode == "" || subscription.StartTime <= 0 || start.After(now) || !end.After(now) {
		return nil, errors.New("Alibaba Cloud did not return an active Personal Token Plan subscription")
	}
	var quotaConfig map[string]struct {
		Monthly *float64 `json:"monthly"`
	}
	if err := h.qwenCloudRequest(ctx, token, "quota-config", map[string]any{}, &quotaConfig); err != nil {
		return nil, err
	}
	quota, found := quotaConfig[subscription.SpecCode]
	if !found || quota.Monthly == nil || !validQwenCreditAmounts(*quota.Monthly, 0) {
		return nil, errors.New("Alibaba Cloud did not report a monthly credit allowance for this subscription plan")
	}
	var usage struct {
		Fraction  *float64 `json:"per1MonthPercentage"`
		ResetTime int64    `json:"per1MonthResetTime"`
	}
	if err := h.qwenCloudRequest(ctx, token, "usage", map[string]any{}, &usage); err != nil {
		return nil, err
	}
	reset := time.UnixMilli(usage.ResetTime)
	if usage.Fraction == nil || math.IsNaN(*usage.Fraction) || math.IsInf(*usage.Fraction, 0) || *usage.Fraction < 0 || *usage.Fraction > 1 || !reset.After(now) || reset.After(end) || !reset.After(start) {
		return nil, errors.New("Alibaba Cloud returned invalid monthly usage or a mismatched subscription cycle")
	}
	used := *quota.Monthly * *usage.Fraction
	return &QwenCreditSnapshot{Edition: "personal", Plan: subscription.SpecCode,
		Total: *quota.Monthly, Used: used, Remaining: *quota.Monthly - used,
		CycleStart: start.UTC(), CycleEnd: reset.UTC(), UpdatedAt: now.UTC(), Source: "alibaba-console",
	}, nil
}

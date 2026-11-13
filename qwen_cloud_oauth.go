package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	qwenCloudSignInOrigin = "https://signin.alibabacloud.com"
	qwenCloudOAuthOrigin  = "https://oauth.alibabacloud.com"
	qwenCloudOAuthClient  = "4103531455503354461"
	qwenOAuthRedirectURI  = "http://127.0.0.1:12345/cli/callback"
)

type qwenOAuthTokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type QwenCloudOAuth struct {
	mu     sync.Mutex
	tokens qwenOAuthTokens
}

func (oauth *QwenCloudOAuth) snapshot() qwenOAuthTokens {
	oauth.mu.Lock()
	defer oauth.mu.Unlock()
	return oauth.tokens
}

func (oauth *QwenCloudOAuth) MarshalJSON() ([]byte, error) {
	return json.Marshal(oauth.snapshot())
}

func (oauth *QwenCloudOAuth) UnmarshalJSON(data []byte) error {
	var tokens qwenOAuthTokens
	if err := json.Unmarshal(data, &tokens); err != nil {
		return err
	}
	oauth.mu.Lock()
	oauth.tokens = tokens
	oauth.mu.Unlock()
	return nil
}

func validQwenOAuthTokens(tokens qwenOAuthTokens) bool {
	return tokens.AccessToken != "" && tokens.RefreshToken != "" && !tokens.ExpiresAt.IsZero() &&
		!strings.ContainsAny(tokens.AccessToken+tokens.RefreshToken, " \t\r\n")
}

func (h *proxyHandler) qwenOAuthRequest(ctx context.Context, operation string, form url.Values, bearer string, output any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, qwenCloudOAuthOrigin+"/v1/"+operation, body)
	if err != nil {
		return errors.New("could not create Alibaba Cloud OAuth request")
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "aliyun-cli")
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := h.transport.RoundTrip(req)
	if err != nil {
		return errors.New("Alibaba Cloud OAuth request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Alibaba Cloud OAuth %s returned HTTP %d; sign in again if authorization has expired", operation, response.StatusCode)
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(output) != nil {
		return errors.New("Alibaba Cloud returned an invalid OAuth response")
	}
	return nil
}

func (h *proxyHandler) fetchQwenOAuthTokens(ctx context.Context, form url.Values, now time.Time) (qwenOAuthTokens, error) {
	form.Set("client_id", qwenCloudOAuthClient)
	var response struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := h.qwenOAuthRequest(ctx, "token", form, "", &response); err != nil {
		return qwenOAuthTokens{}, err
	}
	tokens := qwenOAuthTokens{AccessToken: response.AccessToken, RefreshToken: response.RefreshToken}
	if response.ExpiresIn <= 0 || response.ExpiresIn > 86400 || !strings.EqualFold(response.TokenType, "Bearer") {
		return qwenOAuthTokens{}, errors.New("Alibaba Cloud returned an invalid OAuth token lifetime or type")
	}
	tokens.ExpiresAt = now.Add(time.Duration(response.ExpiresIn) * time.Second).UTC()
	if !validQwenOAuthTokens(tokens) {
		return qwenOAuthTokens{}, errors.New("Alibaba Cloud returned incomplete OAuth credentials")
	}
	return tokens, nil
}

func qwenOAuthCloudTokenKey(endpoint string, tokens qwenOAuthTokens) [32]byte {
	encoded, _ := json.Marshal([3]string{endpoint, "oauth", tokens.RefreshToken})
	return sha256.Sum256(encoded)
}

func (h *proxyHandler) qwenOAuthCloudAccessToken(ctx context.Context, oauth *QwenCloudOAuth, now time.Time) (string, error) {
	if oauth == nil || h.cfg == nil || h.cfg.qwenPlanBase == nil {
		return "", errors.New("Alibaba Cloud OAuth account is not configured")
	}
	endpoint := h.cfg.qwenPlanBase.String()
	key := qwenOAuthCloudTokenKey(endpoint, oauth.snapshot())
	cache := &h.qwenCloudTokens
	if token, found := cache.get(key, now); found {
		return token, nil
	}
	result := cache.requests.DoChan("oauth:"+hex.EncodeToString(key[:]), func() (any, error) {
		oauth.mu.Lock()
		defer oauth.mu.Unlock()
		if !validQwenOAuthTokens(oauth.tokens) {
			return "", errors.New("Alibaba Cloud OAuth credentials are invalid; sign in again")
		}
		currentKey := qwenOAuthCloudTokenKey(endpoint, oauth.tokens)
		if token, found := cache.get(currentKey, now); found {
			return token, nil
		}
		if !oauth.tokens.ExpiresAt.After(now.Add(30 * time.Second)) {
			updated, err := h.fetchQwenOAuthTokens(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {oauth.tokens.RefreshToken}}, now)
			if err != nil {
				return "", err
			}
			oauth.tokens = updated
		}
		var credentials struct {
			AccessKeyID     string `json:"accessKeyId"`
			AccessKeySecret string `json:"accessKeySecret"`
			SecurityToken   string `json:"securityToken"`
			Expiration      string `json:"expiration"`
		}
		if err := h.qwenOAuthRequest(ctx, "exchange", nil, oauth.tokens.AccessToken, &credentials); err != nil {
			return "", err
		}
		expiresAt, err := time.Parse(time.RFC3339, credentials.Expiration)
		if err != nil || !expiresAt.After(now) || credentials.AccessKeyID == "" || credentials.AccessKeySecret == "" || credentials.SecurityToken == "" {
			return "", errors.New("Alibaba Cloud returned invalid temporary account credentials")
		}
		token, err := h.issueQwenCloudAccessToken(ctx, &QwenTokenPlanConfig{AccessKeyID: credentials.AccessKeyID, AccessKeySecret: credentials.AccessKeySecret, SecurityToken: credentials.SecurityToken}, now)
		if err != nil {
			return "", err
		}
		cache.put(qwenOAuthCloudTokenKey(endpoint, oauth.tokens), token, now)
		return token, nil
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-result:
		if result.Err != nil {
			return "", result.Err
		}
		return result.Val.(string), nil
	}
}

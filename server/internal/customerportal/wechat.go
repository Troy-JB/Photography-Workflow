package customerportal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type wechatClient struct {
	appID, appSecret string
	http             *http.Client
	mu               sync.Mutex
	accessToken      string
	accessExpires    time.Time
}

func newWechatClient(appID, appSecret string) *wechatClient {
	return &wechatClient{appID: appID, appSecret: appSecret, http: &http.Client{Timeout: 8 * time.Second}}
}

func (w *wechatClient) exchangeCode(ctx context.Context, code string) (string, error) {
	if w.appID == "" || w.appSecret == "" {
		return "", fmt.Errorf("wechat app credentials are not configured")
	}
	query := url.Values{"appid": {w.appID}, "secret": {w.appSecret}, "js_code": {code}, "grant_type": {"authorization_code"}}
	var response struct {
		OpenID  string `json:"openid"`
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := w.getJSON(ctx, "https://api.weixin.qq.com/sns/jscode2session?"+query.Encode(), &response); err != nil {
		return "", err
	}
	if response.ErrCode != 0 || response.OpenID == "" {
		return "", fmt.Errorf("wechat code exchange: %d %s", response.ErrCode, response.ErrMsg)
	}
	return response.OpenID, nil
}

func (w *wechatClient) exchangePhone(ctx context.Context, phoneCode string) (string, error) {
	token, err := w.getAccessToken(ctx)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]string{"code": phoneCode})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.weixin.qq.com/wxa/business/getuserphonenumber?access_token="+url.QueryEscape(token), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		ErrCode   int    `json:"errcode"`
		ErrMsg    string `json:"errmsg"`
		PhoneInfo struct {
			PhoneNumber string `json:"phoneNumber"`
			CountryCode string `json:"countryCode"`
		} `json:"phone_info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || result.ErrCode != 0 || result.PhoneInfo.PhoneNumber == "" {
		return "", fmt.Errorf("wechat phone exchange: %d %s", result.ErrCode, result.ErrMsg)
	}
	phone := normalizePhone(result.PhoneInfo.PhoneNumber)
	if !strings.HasPrefix(phone, "+") && result.PhoneInfo.CountryCode != "" && result.PhoneInfo.CountryCode != "86" {
		phone = "+" + result.PhoneInfo.CountryCode + phone
	}
	return phone, nil
}

func (w *wechatClient) miniProgramCode(ctx context.Context, scene string) ([]byte, error) {
	if w.appID == "" || w.appSecret == "" {
		return nil, fmt.Errorf("wechat app credentials are not configured")
	}
	token, err := w.getAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{
		"scene":       scene,
		"page":        "pages/invite/invite",
		"width":       430,
		"check_path":  false,
		"env_version": "release",
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.weixin.qq.com/wxa/getwxacodeunlimit?access_token="+url.QueryEscape(token), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("wechat code generation returned HTTP %d", resp.StatusCode)
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		var failure struct {
			ErrCode int    `json:"errcode"`
			ErrMsg  string `json:"errmsg"`
		}
		if err := json.Unmarshal(data, &failure); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("wechat code generation: %d %s", failure.ErrCode, failure.ErrMsg)
	}
	if len(data) < 8 || string(data[:4]) != "\x89PNG" {
		return nil, fmt.Errorf("wechat code generation returned an invalid image")
	}
	return data, nil
}

func (w *wechatClient) getAccessToken(ctx context.Context) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.accessToken != "" && time.Now().Before(w.accessExpires) {
		return w.accessToken, nil
	}
	query := url.Values{"appid": {w.appID}, "secret": {w.appSecret}, "grant_type": {"client_credential"}}
	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
	}
	if err := w.getJSON(ctx, "https://api.weixin.qq.com/cgi-bin/token?"+query.Encode(), &result); err != nil {
		return "", err
	}
	if result.ErrCode != 0 || result.AccessToken == "" {
		return "", fmt.Errorf("wechat access token: %d %s", result.ErrCode, result.ErrMsg)
	}
	if result.ExpiresIn <= 60 {
		return "", fmt.Errorf("wechat access token expires too soon")
	}
	w.accessToken = result.AccessToken
	w.accessExpires = time.Now().Add(time.Duration(result.ExpiresIn-60) * time.Second)
	return w.accessToken, nil
}

func (w *wechatClient) getJSON(ctx context.Context, target string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("wechat returned HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

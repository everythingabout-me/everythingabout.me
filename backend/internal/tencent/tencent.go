// Package tencent 实现腾讯云 API 的 TC3-HMAC-SHA256 签名。
//
// 只覆盖本服务用到的两个接口：SES 发信和 STS 临时凭证。
// 手写签名避免了把整个 tencentcloud-sdk-go 拖进来（几百 MB、上百个服务）。
package tencent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Credentials struct {
	SecretID  string
	SecretKey string
}

// APIError 是腾讯云返回的业务错误。
type APIError struct {
	Code      string
	Message   string
	RequestID string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

// Call 调用一个腾讯云 JSON API，把响应放进 out。
func Call(
	ctx context.Context,
	cred Credentials,
	service, version, region, action string,
	payload any,
	out any,
) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := service + ".tencentcloudapi.com"
	timestamp := time.Now().Unix()
	date := time.Unix(timestamp, 0).UTC().Format("2006-01-02")

	const contentType = "application/json; charset=utf-8"
	canonicalHeaders := "content-type:" + contentType + "\nhost:" + endpoint + "\n"
	signedHeaders := "content-type;host"
	payloadHash := sha256Hex(body)

	canonicalRequest := strings.Join([]string{
		http.MethodPost,
		"/",
		"",
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	stringToSign := strings.Join([]string{
		"TC3-HMAC-SHA256",
		strconv.FormatInt(timestamp, 10),
		date + "/" + service + "/tc3_request",
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	secretDate := hmacSHA256([]byte("TC3"+cred.SecretKey), date)
	secretService := hmacSHA256(secretDate, service)
	secretSigning := hmacSHA256(secretService, "tc3_request")
	signature := hex.EncodeToString(hmacSHA256(secretSigning, stringToSign))

	authorization := fmt.Sprintf(
		"TC3-HMAC-SHA256 Credential=%s/%s/%s/tc3_request, SignedHeaders=%s, Signature=%s",
		cred.SecretID, date, service, signedHeaders, signature,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-TC-Action", action)
	req.Header.Set("X-TC-Version", version)
	req.Header.Set("X-TC-Timestamp", strconv.FormatInt(timestamp, 10))
	if region != "" {
		req.Header.Set("X-TC-Region", region)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}

	var envelope struct {
		Response json.RawMessage `json:"Response"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("腾讯云返回了无法解析的响应（HTTP %d）", resp.StatusCode)
	}
	if len(envelope.Response) == 0 {
		return fmt.Errorf("腾讯云返回了空响应（HTTP %d）", resp.StatusCode)
	}

	var probe struct {
		Error *struct {
			Code      string `json:"Code"`
			Message   string `json:"Message"`
			RequestID string `json:"RequestId"`
		} `json:"Error"`
		RequestID string `json:"RequestId"`
	}
	if err := json.Unmarshal(envelope.Response, &probe); err != nil {
		return fmt.Errorf("腾讯云返回了无法解析的响应（HTTP %d）", resp.StatusCode)
	}
	if probe.Error != nil {
		return &APIError{
			Code:      probe.Error.Code,
			Message:   probe.Error.Message,
			RequestID: probe.Error.RequestID,
		}
	}
	if resp.StatusCode >= 400 {
		return &APIError{Code: "HTTP" + strconv.Itoa(resp.StatusCode), Message: strings.TrimSpace(string(raw))}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(envelope.Response, out)
}

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(value))
	return mac.Sum(nil)
}

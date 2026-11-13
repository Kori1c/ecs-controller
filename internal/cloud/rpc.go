package cloud

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// RPCClient implements Alibaba Cloud's common RPC signing protocol. Keeping it
// here avoids coupling the controller to a large SDK and makes requests easy
// to fake in tests.
type RPCClient struct {
	HTTPClient *http.Client
	Endpoint   string
	Version    string
	Product    string
	AccessKey  string
	Secret     string
}

func (c *RPCClient) Call(ctx context.Context, action string, params map[string]string) (map[string]any, error) {
	if c == nil || c.Endpoint == "" || c.AccessKey == "" || c.Secret == "" {
		return nil, fmt.Errorf("cloud client is not configured")
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		result, err := c.callOnce(ctx, action, params)
		if err == nil {
			return result, nil
		}
		lastErr = err
		// A timeout after a mutating RPC is ambiguous: repeating it can create
		// duplicate VPCs, EIPs, security groups, or ECS instances. Only retry
		// actions whose repeated execution is read-only (or explicitly safe).
		if !rpcActionRetryable(action) || !retryableRPCError(err) || attempt == 2 {
			break
		}
		delay := time.Duration(1<<attempt) * time.Second
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func rpcActionRetryable(action string) bool {
	action = strings.ToLower(strings.TrimSpace(action))
	for _, prefix := range []string{"describe", "list", "query", "get", "check"} {
		if strings.HasPrefix(action, prefix) {
			return true
		}
	}
	return false
}

func (c *RPCClient) callOnce(ctx context.Context, action string, params map[string]string) (map[string]any, error) {
	if c == nil || c.Endpoint == "" || c.AccessKey == "" || c.Secret == "" {
		return nil, fmt.Errorf("cloud client is not configured")
	}
	values := url.Values{}
	values.Set("AccessKeyId", c.AccessKey)
	values.Set("Action", action)
	values.Set("Format", "JSON")
	values.Set("SignatureMethod", "HMAC-SHA1")
	values.Set("SignatureNonce", nonce())
	values.Set("SignatureVersion", "1.0")
	values.Set("Timestamp", time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	values.Set("Version", c.Version)
	for k, v := range params {
		if v != "" {
			values.Set(k, v)
		}
	}

	canonical := canonicalQuery(values)
	stringToSign := "GET&%2F&" + percentEncode(canonical)
	h := hmac.New(sha1.New, []byte(c.Secret+"&"))
	_, _ = h.Write([]byte(stringToSign))
	values.Set("Signature", base64.StdEncoding.EncodeToString(h.Sum(nil)))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Endpoint+"?"+values.Encode(), nil)
	if err != nil {
		return nil, err
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("cloud response: %s: %w", strings.TrimSpace(string(body)), err)
	}
	code := stringValue(result["Code"])
	if resp.StatusCode >= 400 || !rpcSuccessCode(code) {
		return result, &APIError{Code: stringValue(result["Code"]), Message: stringValue(result["Message"]), HTTPStatus: resp.StatusCode}
	}
	return result, nil
}

func rpcSuccessCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "", "200", "success", "ok":
		return true
	default:
		return false
	}
}

func retryableRPCError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		code := strings.ToLower(apiErr.Code)
		return apiErr.HTTPStatus >= 500 || strings.Contains(code, "throttl") || strings.Contains(code, "too_many_requests") || strings.Contains(code, "internalerror")
	}
	return true
}

type APIError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr != nil && !IsCredentialError(err) && !IsPermissionError(err) && (strings.Contains(strings.ToLower(apiErr.Code), "notfound") || strings.Contains(strings.ToLower(apiErr.Code), "not_found"))
}

func IsCredentialError(err error) bool {
	return hasAPIErrorCode(err, "invalidaccesskeyid", "invalidaccesskeysecret", "signaturedoesnotmatch", "invalidsignature", "signatureexpired", "invalidsecuritytoken", "securitytokenexpired", "missingsecuritytoken")
}

func IsPermissionError(err error) bool {
	return hasAPIErrorCode(err, "forbidden", "unauthorized", "unauthorizedoperation", "nopermission")
}

func hasAPIErrorCode(err error, families ...string) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil {
		return false
	}
	// Transport errors contain signed URLs; AccessKeyId and Signature query
	// parameters are not evidence of a provider authentication failure.
	code := strings.ToLower(strings.TrimSpace(apiErr.Code))
	for _, family := range families {
		if code == family || strings.HasPrefix(code, family+".") {
			return true
		}
	}
	return false
}

func (e *APIError) Error() string { return fmt.Sprintf("aliyun %s: %s", e.Code, e.Message) }

func nonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func canonicalQuery(values url.Values) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		if key != "Signature" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		vals := append([]string(nil), values[key]...)
		sort.Strings(vals)
		for _, value := range vals {
			parts = append(parts, percentEncode(key)+"="+percentEncode(value))
		}
	}
	return strings.Join(parts, "&")
}

func percentEncode(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(url.QueryEscape(value), "+", "%20"), "%7E", "~")
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%g", v)
	default:
		return fmt.Sprint(v)
	}
}

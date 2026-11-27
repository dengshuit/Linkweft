package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"linkweft/internal/retrieval"
)

const maxResponseBytes = 16 << 20

func operationURL(baseURL, defaultBaseURL, path string) string {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func fixedClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	fixed := *client
	fixed.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &fixed
}

func postJSON(ctx context.Context, client *http.Client, endpoint, authHeader, authValue string, payload, output any) *retrieval.Error {
	return postJSONWithErrorMapping(ctx, client, endpoint, authHeader, authValue, payload, output, nil)
}

func postJSONWithErrorMapping(
	ctx context.Context, client *http.Client, endpoint, authHeader, authValue string, payload, output any,
	mapError func(int, io.Reader) *retrieval.Error,
) *retrieval.Error {
	body, err := json.Marshal(payload)
	if err != nil {
		return &retrieval.Error{Code: "upstream_error", Message: "cannot encode provider request"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return &retrieval.Error{Code: "upstream_error", Message: "cannot create provider request"}
	}
	req.Header.Set(authHeader, authValue)
	req.Header.Set("Content-Type", "application/json")
	return doJSON(client, req, output, mapError)
}

func doJSON(client *http.Client, req *http.Request, output any, mapError func(int, io.Reader) *retrieval.Error) *retrieval.Error {
	resp, err := client.Do(req)
	if err != nil {
		return requestError(err, "provider request failed")
	}
	defer resp.Body.Close()
	var issue *retrieval.Error
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if mapError != nil {
			issue = mapError(resp.StatusCode, resp.Body)
		}
		if issue == nil {
			switch resp.StatusCode {
			case http.StatusUnauthorized, http.StatusForbidden:
				issue = &retrieval.Error{Code: "auth_failed", Message: "provider rejected the API credentials"}
			case http.StatusTooManyRequests:
				issue = &retrieval.Error{Code: "rate_limited", Message: "provider rate limit reached"}
			default:
				issue = &retrieval.Error{Code: "upstream_error", Message: "provider returned an unsuccessful HTTP status"}
			}
		}
	} else {
		issue = readJSON(resp.Body, output)
	}
	if issue != nil {
		issue.HTTPStatus = &resp.StatusCode
		issue.RetryAfterMS = retryAfterMS(resp.Header.Get("Retry-After"), time.Now())
	}
	return issue
}

func retryAfterMS(value string, now time.Time) *int64 {
	value = strings.TrimSpace(value)
	seconds, err := strconv.ParseUint(value, 10, 64)
	if err == nil && seconds <= math.MaxInt64/1000 {
		delay := int64(seconds) * 1000
		return &delay
	}
	date, err := http.ParseTime(value)
	if err != nil {
		return nil
	}
	delay := max(int64(0), date.Sub(now).Milliseconds())
	return &delay
}

func withTargetHTTPStatus(issue *retrieval.Error, status int) *retrieval.Error {
	if issue != nil && status >= 100 && status <= 599 {
		issue.TargetHTTPStatus = &status
	}
	return issue
}

func readJSON(body io.Reader, output any) *retrieval.Error {
	data, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return requestError(err, "cannot read provider response")
	}
	if len(data) > maxResponseBytes || json.Unmarshal(data, output) != nil {
		return &retrieval.Error{Code: "invalid_response", Message: "provider returned an invalid or oversized JSON response"}
	}
	return nil
}

func requestError(err error, message string) *retrieval.Error {
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return &retrieval.Error{Code: "provider_timeout", Message: "provider request timed out"}
	}
	return &retrieval.Error{Code: "upstream_error", Message: message}
}

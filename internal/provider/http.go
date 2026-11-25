package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

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
	resp, err := client.Do(req)
	if err != nil {
		return requestError(err, "provider request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if mapError != nil {
			if issue := mapError(resp.StatusCode, resp.Body); issue != nil {
				return issue
			}
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return &retrieval.Error{Code: "auth_failed", Message: "provider rejected the API credentials"}
		case http.StatusTooManyRequests:
			return &retrieval.Error{Code: "rate_limited", Message: "provider rate limit reached"}
		default:
			return &retrieval.Error{Code: "upstream_error", Message: "provider returned an unsuccessful HTTP status"}
		}
	}
	return readJSON(resp.Body, output)
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

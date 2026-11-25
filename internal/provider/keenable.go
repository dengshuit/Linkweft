package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"linkweft/internal/retrieval"
)

const keenableSearchURL = "https://api.keenable.ai/v1/search"
const keenableFetchURL = "https://api.keenable.ai/v1/fetch"

type keenable struct {
	key      string
	endpoint string
	client   *http.Client
}

func NewKeenable(apiKey string, client *http.Client, endpoint string) retrieval.SearchProvider {
	if endpoint == "" {
		endpoint = keenableSearchURL
	}
	return retrieval.SearchProvider{
		Searcher:   &keenable{key: apiKey, client: fixedClient(client), endpoint: endpoint},
		MaxResults: 50,
	}
}

func (k *keenable) Search(ctx context.Context, request retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
	payload := struct {
		Query        string `json:"query"`
		MaxResults   int    `json:"max_results"`
		DisableCards bool   `json:"disable_cards"`
	}{Query: request.Query, MaxResults: request.Limit, DisableCards: true}
	var response searchResponse
	if issue := k.post(ctx, payload, &response); issue != nil {
		return nil, issue
	}
	return response.candidates("snippet")
}

func NewKeenableFetch(apiKey string, client *http.Client, endpoint string) retrieval.FetchProvider {
	if endpoint == "" {
		endpoint = keenableFetchURL
	}
	return retrieval.FetchProvider{Fetcher: &keenable{key: apiKey, client: fixedClient(client), endpoint: endpoint}}
}

func (k *keenable) Fetch(ctx context.Context, address string) (*retrieval.Document, *retrieval.Error) {
	payload := struct {
		URLs [1]string `json:"urls"`
	}{URLs: [1]string{address}}
	var response struct {
		Results []json.RawMessage `json:"results"`
	}
	if issue := k.post(ctx, payload, &response); issue != nil {
		return nil, issue
	}
	if len(response.Results) != 1 {
		return nil, &retrieval.Error{Code: "invalid_response", Message: "provider did not return exactly one fetch result"}
	}

	var status *struct {
		Error *keenableError `json:"error"`
	}
	result := response.Results[0]
	if err := json.Unmarshal(result, &status); err != nil || status == nil {
		return nil, &retrieval.Error{Code: "invalid_response", Message: "provider returned an invalid fetch result"}
	}
	if status.Error != nil {
		if issue := status.Error.retrievalError(); issue != nil {
			return nil, issue
		}
		return nil, &retrieval.Error{Code: "upstream_error", Message: "provider reported content retrieval failure for the requested URL"}
	}

	var page struct {
		URL       *string         `json:"url"`
		Title     json.RawMessage `json:"title"`
		Content   string          `json:"content"`
		Truncated *bool           `json:"truncated"`
		Usage     struct {
			SKU string `json:"sku"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(result, &page); err != nil {
		return nil, &retrieval.Error{Code: "invalid_response", Message: "provider returned an invalid fetch result"}
	}
	freshness := "unknown"
	switch page.Usage.SKU {
	case "fetch":
		freshness = "cached"
	case "fetch.live":
		freshness = "live"
	}
	return &retrieval.Document{
		ReturnedURL: page.URL,
		FinalURL:    page.URL,
		Title:       optionalText(page.Title),
		Content: retrieval.Content{
			Text: page.Content, Format: "markdown", UpstreamTruncated: page.Truncated, Freshness: freshness,
		},
	}, nil
}

func (k *keenable) post(ctx context.Context, payload, output any) *retrieval.Error {
	return postJSONWithErrorMapping(ctx, k.client, k.endpoint, "X-API-Key", k.key, payload, output, keenableHTTPError)
}

type keenableError struct {
	Code string `json:"code"`
}

func (e keenableError) retrievalError() *retrieval.Error {
	switch e.Code {
	case "target_blocked", "target_paywalled", "private_url_not_allowed":
		return &retrieval.Error{Code: "content_blocked", Message: "provider reported that access to the requested URL was blocked"}
	case "credentials_forbidden":
		return &retrieval.Error{Code: "auth_failed", Message: "provider rejected the API credentials"}
	case "target_rate_limited":
		return &retrieval.Error{Code: "rate_limited", Message: "provider reported a rate limit for the requested URL"}
	}
	return nil
}

func keenableHTTPError(status int, body io.Reader) *retrieval.Error {
	// Target errors may share a status with credential failures or invalid requests.
	if status != http.StatusForbidden && status != http.StatusUnprocessableEntity {
		return nil
	}
	var failure keenableError
	if issue := readJSON(body, &failure); issue != nil {
		if issue.Code == "invalid_response" {
			return nil // Fall back to the HTTP status if there is no usable error body.
		}
		return issue
	}
	return failure.retrievalError()
}

package provider

import (
	"context"
	"encoding/json"
	"net/http"

	"linkweft/internal/retrieval"
)

const anySearchBaseURL = "https://api.anysearch.com"

type anySearch struct {
	key      string
	endpoint string
	client   *http.Client
}

func NewAnySearch(apiKey string, client *http.Client, baseURL string) retrieval.SearchProvider {
	endpoint := operationURL(baseURL, anySearchBaseURL, "/v1/search")
	return retrieval.SearchProvider{
		Searcher:   &anySearch{key: apiKey, client: fixedClient(client), endpoint: endpoint},
		MaxResults: 10,
	}
}

func (a *anySearch) Search(ctx context.Context, request retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
	payload := struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}{Query: request.Query, MaxResults: request.Limit}
	var data searchResponse
	if issue := a.post(ctx, payload, &data); issue != nil {
		return nil, issue
	}
	return data.candidates("snippet+content")
}

func NewAnySearchFetch(apiKey string, client *http.Client, baseURL string) retrieval.FetchProvider {
	endpoint := operationURL(baseURL, anySearchBaseURL, "/v1/extract")
	return retrieval.FetchProvider{Fetcher: &anySearch{key: apiKey, client: fixedClient(client), endpoint: endpoint}}
}

func (a *anySearch) Fetch(ctx context.Context, address string) (*retrieval.Document, *retrieval.Error) {
	payload := struct {
		URL string `json:"url"`
	}{URL: address}
	var data struct {
		URL     json.RawMessage `json:"url"`
		Title   json.RawMessage `json:"title"`
		Content json.RawMessage `json:"content"`
	}
	if issue := a.post(ctx, payload, &data); issue != nil {
		return nil, issue
	}
	var content *string
	if data.Content == nil {
		return nil, nil
	}
	if json.Unmarshal(data.Content, &content) != nil || content == nil {
		return nil, anySearchInvalidResponse()
	}
	// Extract echoes the submitted URL even when it follows a page redirect.
	return &retrieval.Document{
		ReturnedURL: optionalText(data.URL),
		Title:       optionalText(data.Title),
		Content:     retrieval.Content{Text: *content, Format: "unknown", Freshness: "unknown"},
	}, nil
}

func (a *anySearch) post(ctx context.Context, payload, data any) *retrieval.Error {
	var response struct {
		Code      *int            `json:"code"`
		ErrorCode json.RawMessage `json:"error_code"`
		Data      json.RawMessage `json:"data"`
	}
	if issue := postJSON(ctx, a.client, a.endpoint, "Authorization", "Bearer "+a.key, payload, &response); issue != nil {
		return issue
	}
	if response.Code == nil {
		return anySearchInvalidResponse()
	}
	var errorCode string
	if *response.Code != 0 || (json.Unmarshal(response.ErrorCode, &errorCode) == nil &&
		(errorCode == "extract_failed" || errorCode == "invalid_extract_url")) {
		return &retrieval.Error{Code: "upstream_error", Message: "provider reported an unsuccessful response"}
	}
	if len(response.Data) == 0 || response.Data[0] != '{' || json.Unmarshal(response.Data, data) != nil {
		return anySearchInvalidResponse()
	}
	return nil
}

func anySearchInvalidResponse() *retrieval.Error {
	return &retrieval.Error{Code: "invalid_response", Message: "provider returned an invalid response structure"}
}

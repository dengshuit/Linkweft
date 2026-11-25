package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"linkweft/internal/retrieval"
)

type firecrawl struct {
	key      string
	endpoint string
	client   *http.Client
}

func NewFirecrawl(apiKey string, client *http.Client, baseURL string) retrieval.SearchProvider {
	endpoint := operationURL(baseURL, "https://api.firecrawl.dev", "/v2/search")
	return retrieval.SearchProvider{
		Searcher:   &firecrawl{key: apiKey, client: fixedClient(client), endpoint: endpoint},
		MaxResults: 100,
	}
}

func (f *firecrawl) Search(ctx context.Context, request retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
	payload := struct {
		Query      string    `json:"query"`
		Limit      int       `json:"limit"`
		Sources    [1]string `json:"sources"`
		Highlights bool      `json:"highlights"`
	}{Query: request.Query, Limit: request.Limit, Sources: [1]string{"web"}, Highlights: false}
	var data struct {
		Web json.RawMessage `json:"web"`
	}
	if issue := f.post(ctx, payload, &data, firecrawlHTTPError); issue != nil {
		return nil, issue
	}
	var items []json.RawMessage
	if data.Web != nil && (json.Unmarshal(data.Web, &items) != nil || items == nil) {
		return nil, firecrawlInvalidResponse()
	}
	results := make([]retrieval.Candidate, len(items))
	for i, item := range items {
		var result struct {
			URL         string          `json:"url"`
			Title       json.RawMessage `json:"title"`
			Description json.RawMessage `json:"description"`
			Position    json.RawMessage `json:"position"`
		}
		if err := json.Unmarshal(item, &result); err != nil {
			continue
		}
		rank := i + 1
		var position int
		if json.Unmarshal(result.Position, &position) == nil && position > 0 {
			rank = position
		}
		results[i] = retrieval.Candidate{
			URL: result.URL, Title: optionalText(result.Title), Snippet: optionalText(result.Description), Rank: rank,
		}
	}
	return results, nil
}

func NewFirecrawlFetch(apiKey string, client *http.Client, baseURL string) retrieval.FetchProvider {
	endpoint := operationURL(baseURL, "https://api.firecrawl.dev", "/v2/scrape")
	return retrieval.FetchProvider{Fetcher: &firecrawl{key: apiKey, client: fixedClient(client), endpoint: endpoint}}
}

func (f *firecrawl) Fetch(ctx context.Context, address string) (*retrieval.Document, *retrieval.Error) {
	payload := struct {
		URL             string    `json:"url"`
		Formats         [1]string `json:"formats"`
		OnlyMainContent bool      `json:"onlyMainContent"`
	}{URL: address, Formats: [1]string{"markdown"}, OnlyMainContent: true}
	var data struct {
		Markdown json.RawMessage `json:"markdown"`
		Metadata json.RawMessage `json:"metadata"`
	}
	if issue := f.post(ctx, payload, &data, firecrawlScrapeHTTPError); issue != nil {
		return nil, issue
	}
	metadata := &struct {
		StatusCode json.RawMessage `json:"statusCode"`
		Error      json.RawMessage `json:"error"`
		URL        *string         `json:"url"`
		Title      string          `json:"title"`
		CacheState string          `json:"cacheState"`
	}{}
	if data.Metadata != nil && (json.Unmarshal(data.Metadata, &metadata) != nil || metadata == nil) {
		return nil, firecrawlInvalidResponse()
	}
	var statusCode int
	if raw := metadata.StatusCode; raw != nil && (string(raw) == "null" || json.Unmarshal(raw, &statusCode) != nil) {
		return nil, firecrawlInvalidResponse()
	}
	var pageError string
	if raw := metadata.Error; raw != nil && (string(raw) == "null" || json.Unmarshal(raw, &pageError) != nil) {
		return nil, firecrawlInvalidResponse()
	}
	if pageError != "" || statusCode >= 400 {
		switch statusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, firecrawlContentBlocked()
		case http.StatusTooManyRequests:
			return nil, &retrieval.Error{Code: "rate_limited", Message: "provider reported a rate limit for the requested URL"}
		default:
			return nil, &retrieval.Error{Code: "upstream_error", Message: "provider reported content retrieval failure for the requested URL"}
		}
	}

	var markdown string
	if data.Markdown != nil {
		var value *string
		if json.Unmarshal(data.Markdown, &value) != nil || value == nil {
			return nil, firecrawlInvalidResponse()
		}
		markdown = *value
	}
	freshness := "unknown"
	switch metadata.CacheState {
	case "hit":
		freshness = "cached"
	case "miss":
		freshness = "live"
	}
	return &retrieval.Document{
		ReturnedURL: metadata.URL,
		FinalURL:    metadata.URL,
		Title:       &metadata.Title,
		Content:     retrieval.Content{Text: markdown, Format: "markdown", Freshness: freshness},
	}, nil
}

func (f *firecrawl) post(
	ctx context.Context, payload, data any, mapError func(int, io.Reader) *retrieval.Error,
) *retrieval.Error {
	var response struct {
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if issue := postJSONWithErrorMapping(ctx, f.client, f.endpoint, "Authorization", "Bearer "+f.key, payload, &response, mapError); issue != nil {
		return issue
	}
	if response.Success == nil {
		return firecrawlInvalidResponse()
	}
	if !*response.Success {
		return &retrieval.Error{Code: "upstream_error", Message: "provider reported an unsuccessful response"}
	}
	if len(response.Data) == 0 || response.Data[0] != '{' || json.Unmarshal(response.Data, data) != nil {
		return firecrawlInvalidResponse()
	}
	return nil
}

func firecrawlInvalidResponse() *retrieval.Error {
	return &retrieval.Error{Code: "invalid_response", Message: "provider returned an invalid response structure"}
}

func firecrawlHTTPError(status int, _ io.Reader) *retrieval.Error {
	if status == http.StatusRequestTimeout {
		return &retrieval.Error{Code: "provider_timeout", Message: "provider request timed out"}
	}
	return nil
}

func firecrawlScrapeHTTPError(status int, body io.Reader) *retrieval.Error {
	if status != http.StatusForbidden {
		return firecrawlHTTPError(status, body)
	}
	// Scrape uses 403 for target restrictions as well as credential rejection.
	var failure struct {
		Code string `json:"code"`
	}
	if issue := readJSON(body, &failure); issue != nil {
		if issue.Code == "invalid_response" {
			return nil
		}
		return issue
	}
	switch failure.Code {
	case "UNSUPPORTED_SITE", "SAFE_MODE_BLOCKED":
		return firecrawlContentBlocked()
	}
	return nil
}

func firecrawlContentBlocked() *retrieval.Error {
	return &retrieval.Error{Code: "content_blocked", Message: "provider reported that access to the requested URL was blocked"}
}

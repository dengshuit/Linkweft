package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"linkweft/internal/retrieval"
)

const (
	tinyFishSearchBaseURL = "https://api.search.tinyfish.ai"
	tinyFishFetchBaseURL  = "https://api.fetch.tinyfish.ai"
)

type tinyFish struct {
	key      string
	endpoint string
	client   *http.Client
}

func NewTinyFish(apiKey string, client *http.Client, baseURL string) retrieval.SearchProvider {
	endpoint := operationURL(baseURL, tinyFishSearchBaseURL, "/")
	return retrieval.SearchProvider{
		Searcher:   &tinyFish{key: apiKey, client: fixedClient(client), endpoint: endpoint},
		MaxResults: 0,
	}
}

func (a *tinyFish) Search(ctx context.Context, request retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
	query := url.Values{"query": {request.Query}, "page": {"0"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, &retrieval.Error{Code: "upstream_error", Message: "cannot create provider request"}
	}
	req.Header.Set("X-API-Key", a.key)
	var response searchResponse
	if issue := doJSON(a.client, req, &response, nil); issue != nil {
		return nil, issue
	}
	if response.Results == nil {
		return nil, tinyFishInvalidResponse()
	}
	candidates := make([]retrieval.Candidate, len(response.Results))
	for i, item := range response.Results {
		var result struct {
			URL      string          `json:"url"`
			Title    json.RawMessage `json:"title"`
			Snippet  json.RawMessage `json:"snippet"`
			Position json.RawMessage `json:"position"`
		}
		if json.Unmarshal(item, &result) != nil {
			continue
		}
		var rank int
		if json.Unmarshal(result.Position, &rank) != nil || rank <= 0 {
			rank = i + 1
		}
		candidates[i] = retrieval.Candidate{
			URL: result.URL, Title: optionalText(result.Title), Snippet: optionalText(result.Snippet), Rank: rank,
		}
	}
	return candidates, nil
}

func NewTinyFishFetch(apiKey string, client *http.Client, baseURL string) retrieval.FetchProvider {
	endpoint := operationURL(baseURL, tinyFishFetchBaseURL, "/")
	return retrieval.FetchProvider{Fetcher: &tinyFish{key: apiKey, client: fixedClient(client), endpoint: endpoint}}
}

type tinyFishResult struct {
	URL      string          `json:"url"`
	FinalURL json.RawMessage `json:"final_url"`
	Title    json.RawMessage `json:"title"`
	Text     json.RawMessage `json:"text"`
}

func (a *tinyFish) Fetch(ctx context.Context, address string) (*retrieval.Document, *retrieval.Error) {
	payload := struct {
		URLs   []string `json:"urls"`
		Format string   `json:"format"`
	}{URLs: []string{address}, Format: "markdown"}
	var response struct {
		Results []json.RawMessage `json:"results"`
		Errors  []struct {
			URL    string `json:"url"`
			Code   string `json:"error"`
			Status int    `json:"status"`
		} `json:"errors"`
	}
	if issue := postJSON(ctx, a.client, a.endpoint, "X-API-Key", a.key, payload, &response); issue != nil {
		return nil, issue
	}
	if response.Results == nil || response.Errors == nil {
		return nil, tinyFishInvalidResponse()
	}
	for _, failure := range response.Errors {
		if failure.URL != address {
			continue
		}
		if strings.TrimSpace(failure.Code) == "" {
			return nil, tinyFishInvalidResponse()
		}
		return nil, withTargetHTTPStatus(tinyFishFetchFailure(failure.Code, failure.Status), failure.Status)
	}
	var match *tinyFishResult
	for _, item := range response.Results {
		var result tinyFishResult
		if json.Unmarshal(item, &result) != nil {
			return nil, tinyFishInvalidResponse()
		}
		if result.URL != address {
			continue
		}
		if match != nil {
			return nil, tinyFishInvalidResponse()
		}
		match = &result
	}
	var content *string
	if match == nil || json.Unmarshal(match.Text, &content) != nil {
		return nil, tinyFishInvalidResponse()
	}
	if content == nil {
		return nil, nil
	}
	return &retrieval.Document{
		ReturnedURL: &match.URL,
		FinalURL:    optionalText(match.FinalURL),
		Title:       optionalText(match.Title),
		Content:     retrieval.Content{Text: *content, Format: "markdown", Freshness: "unknown"},
	}, nil
}

func tinyFishFetchFailure(code string, status int) *retrieval.Error {
	switch code {
	case "bot_blocked", "login_required":
		return &retrieval.Error{Code: "content_blocked", Message: "provider reported that the target page blocked access"}
	case "target_http_error":
		switch status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return &retrieval.Error{Code: "content_blocked", Message: "provider reported that the target page blocked access"}
		case http.StatusTooManyRequests:
			return &retrieval.Error{Code: "rate_limited", Message: "provider reported that the target page rate limited the request"}
		}
	case "timeout":
		return &retrieval.Error{Code: "provider_timeout", Message: "provider reported that fetching the target page timed out"}
	case "empty_content":
		return nil
	}
	return &retrieval.Error{Code: "upstream_error", Message: "provider reported an unsuccessful response for the target page"}
}

func tinyFishInvalidResponse() *retrieval.Error {
	return &retrieval.Error{Code: "invalid_response", Message: "provider returned an invalid response structure"}
}

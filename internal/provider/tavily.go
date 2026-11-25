package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"linkweft/internal/retrieval"
)

const tavilySearchURL = "https://api.tavily.com/search"
const tavilyExtractURL = "https://api.tavily.com/extract"

type tavily struct {
	key      string
	endpoint string
	client   *http.Client
}

func NewTavily(apiKey string, client *http.Client, endpoint string) retrieval.SearchProvider {
	if endpoint == "" {
		endpoint = tavilySearchURL
	}
	return retrieval.SearchProvider{
		Searcher:   &tavily{key: apiKey, client: fixedClient(client), endpoint: endpoint},
		MaxResults: 20,
	}
}

type tavilyRequest struct {
	Query                    string `json:"query"`
	MaxResults               int    `json:"max_results"`
	SearchDepth              string `json:"search_depth"`
	Topic                    string `json:"topic"`
	AutoParameters           bool   `json:"auto_parameters"`
	IncludeAnswer            bool   `json:"include_answer"`
	IncludeRawContent        bool   `json:"include_raw_content"`
	IncludeImages            bool   `json:"include_images"`
	IncludeImageDescriptions bool   `json:"include_image_descriptions"`
}

func (t *tavily) Search(ctx context.Context, request retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
	payload := tavilyRequest{Query: request.Query, MaxResults: request.Limit, SearchDepth: "basic", Topic: "general"}
	var response searchResponse
	if err := postJSON(ctx, t.client, t.endpoint, "Authorization", "Bearer "+t.key, payload, &response); err != nil {
		return nil, err
	}
	return response.candidates(true)
}

func NewTavilyFetch(apiKey string, client *http.Client, endpoint string) retrieval.FetchProvider {
	if endpoint == "" {
		endpoint = tavilyExtractURL
	}
	return retrieval.FetchProvider{Fetcher: &tavily{key: apiKey, client: fixedClient(client), endpoint: endpoint}}
}

func (t *tavily) Fetch(ctx context.Context, address string) (*retrieval.Document, *retrieval.Error) {
	payload := struct {
		URLs         [1]string `json:"urls"`
		Format       string    `json:"format"`
		ExtractDepth string    `json:"extract_depth"`
	}{URLs: [1]string{address}, Format: "markdown", ExtractDepth: "basic"}
	var response struct {
		Results       json.RawMessage   `json:"results"`
		FailedResults []json.RawMessage `json:"failed_results"`
	}
	if issue := postJSON(ctx, t.client, t.endpoint, "Authorization", "Bearer "+t.key, payload, &response); issue != nil {
		return nil, issue
	}
	if len(response.FailedResults) > 0 {
		return nil, &retrieval.Error{Code: "upstream_error", Message: "provider reported extraction failure for the requested URL"}
	}
	var results []struct {
		URL        *string         `json:"url"`
		Title      json.RawMessage `json:"title"`
		RawContent string          `json:"raw_content"`
	}
	if err := json.Unmarshal(response.Results, &results); err != nil || results == nil || len(results) > 1 {
		return nil, &retrieval.Error{Code: "invalid_response", Message: "provider returned invalid or ambiguous extraction results"}
	}
	if len(results) == 0 || strings.TrimSpace(results[0].RawContent) == "" {
		return nil, nil
	}
	result := results[0]
	return &retrieval.Document{
		ReturnedURL: result.URL,
		Title:       optionalText(result.Title),
		Content:     retrieval.Content{Text: result.RawContent, Format: "markdown", Freshness: "unknown"},
	}, nil
}

package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"linkweft/internal/retrieval"
)

const exaSearchURL = "https://api.exa.ai/search"
const exaContentsURL = "https://api.exa.ai/contents"

type exa struct {
	key      string
	endpoint string
	client   *http.Client
}

func NewExa(apiKey string, client *http.Client, endpoint string) retrieval.SearchProvider {
	if endpoint == "" {
		endpoint = exaSearchURL
	}
	return retrieval.SearchProvider{
		Searcher:   &exa{key: apiKey, client: fixedClient(client), endpoint: endpoint},
		MaxResults: 100,
	}
}

type exaRequest struct {
	Query      string `json:"query"`
	NumResults int    `json:"numResults"`
	Type       string `json:"type"`
	Contents   struct {
		Text       bool `json:"text"`
		Highlights bool `json:"highlights"`
	} `json:"contents"`
}

func (e *exa) Search(ctx context.Context, request retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
	payload := exaRequest{Query: request.Query, NumResults: request.Limit, Type: "auto"}
	var response searchResponse
	if err := postJSON(ctx, e.client, e.endpoint, "x-api-key", e.key, payload, &response); err != nil {
		return nil, err
	}
	return response.candidates(false)
}

func NewExaFetch(apiKey string, client *http.Client, endpoint string) retrieval.FetchProvider {
	if endpoint == "" {
		endpoint = exaContentsURL
	}
	return retrieval.FetchProvider{Fetcher: &exa{key: apiKey, client: fixedClient(client), endpoint: endpoint}}
}

type exaContentStatus struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Source string `json:"source"`
	Error  *struct {
		HTTPStatusCode int `json:"httpStatusCode"`
	} `json:"error"`
}

func (e *exa) Fetch(ctx context.Context, address string) (*retrieval.Document, *retrieval.Error) {
	payload := struct {
		URLs [1]string `json:"urls"`
		Text bool      `json:"text"`
	}{URLs: [1]string{address}, Text: true}
	var response struct {
		Results  json.RawMessage    `json:"results"`
		Statuses []exaContentStatus `json:"statuses"`
	}
	if issue := postJSON(ctx, e.client, e.endpoint, "x-api-key", e.key, payload, &response); issue != nil {
		return nil, issue
	}
	freshness, issue := exaContentFreshness(address, response.Statuses)
	if issue != nil {
		return nil, issue
	}
	var results []struct {
		URL   *string         `json:"url"`
		Title json.RawMessage `json:"title"`
		Text  string          `json:"text"`
	}
	if err := json.Unmarshal(response.Results, &results); err != nil || results == nil {
		return nil, &retrieval.Error{Code: "invalid_response", Message: "provider response is missing a valid results array"}
	}
	var document *retrieval.Document
	for _, result := range results {
		if strings.TrimSpace(result.Text) == "" {
			continue
		}
		if document != nil {
			return nil, &retrieval.Error{Code: "invalid_response", Message: "provider returned multiple bodies for the requested URL"}
		}
		document = &retrieval.Document{
			ReturnedURL: result.URL,
			Title:       optionalText(result.Title),
			Content:     retrieval.Content{Text: result.Text, Format: "markdown", Freshness: freshness},
		}
	}
	return document, nil
}

func exaContentFreshness(address string, statuses []exaContentStatus) (string, *retrieval.Error) {
	freshness := "unknown"
	var status string
	var failure *retrieval.Error
	for _, entry := range statuses {
		if entry.ID != address {
			continue
		}
		if (status != "" && status != entry.Status) || (entry.Status == "success" && entry.Error != nil) {
			return "", &retrieval.Error{Code: "invalid_response", Message: "provider returned contradictory URL statuses"}
		}
		status = entry.Status
		switch entry.Status {
		case "success":
			var source string
			switch entry.Source {
			case "cached":
				source = "cached"
			case "crawled":
				source = "live"
			}
			if source == "" {
				continue
			}
			if freshness != "unknown" && source != freshness {
				return "", &retrieval.Error{Code: "invalid_response", Message: "provider returned contradictory content sources"}
			}
			freshness = source
		case "error":
			if entry.Error != nil && (entry.Error.HTTPStatusCode == http.StatusForbidden || entry.Error.HTTPStatusCode == http.StatusUnauthorized) {
				failure = &retrieval.Error{Code: "content_blocked", Message: "provider reported that access to the requested URL was blocked"}
			} else if failure == nil {
				failure = &retrieval.Error{Code: "upstream_error", Message: "provider reported content retrieval failure for the requested URL"}
			}
		default:
			return "", &retrieval.Error{Code: "invalid_response", Message: "provider returned an invalid URL status"}
		}
	}
	return freshness, failure
}

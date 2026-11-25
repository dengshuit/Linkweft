package provider

import (
	"encoding/json"
	"strings"

	"linkweft/internal/retrieval"
)

type searchResponse struct {
	Results []json.RawMessage `json:"results"`
}

func (r searchResponse) candidates(snippetField string) ([]retrieval.Candidate, *retrieval.Error) {
	if r.Results == nil {
		return nil, &retrieval.Error{Code: "invalid_response", Message: "provider response is missing a results array"}
	}
	results := make([]retrieval.Candidate, len(r.Results))
	for i, item := range r.Results {
		var result struct {
			URL     string          `json:"url"`
			Title   json.RawMessage `json:"title"`
			Content json.RawMessage `json:"content"`
			Snippet json.RawMessage `json:"snippet"`
		}
		if err := json.Unmarshal(item, &result); err != nil {
			continue
		}
		candidate := retrieval.Candidate{URL: result.URL, Title: optionalText(result.Title), Rank: i + 1}
		switch snippetField {
		case "content":
			candidate.Snippet = optionalText(result.Content)
		case "snippet":
			candidate.Snippet = optionalText(result.Snippet)
		}
		results[i] = candidate
	}
	return results, nil
}

func optionalText(raw json.RawMessage) *string {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

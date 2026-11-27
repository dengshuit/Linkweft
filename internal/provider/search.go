package provider

import (
	"encoding/json"
	"slices"
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
			URL        string          `json:"url"`
			Title      json.RawMessage `json:"title"`
			Content    json.RawMessage `json:"content"`
			Snippet    json.RawMessage `json:"snippet"`
			Highlights json.RawMessage `json:"highlights"`
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
		case "highlights":
			var highlights []string
			if json.Unmarshal(result.Highlights, &highlights) == nil {
				highlights = slices.DeleteFunc(highlights, func(text string) bool { return strings.TrimSpace(text) == "" })
				if len(highlights) > 0 {
					text := strings.Join(highlights, "\n")
					candidate.Snippet = &text
				}
			}
		case "snippet+content":
			candidate.Snippet = optionalText(result.Snippet)
			if content := optionalText(result.Content); content != nil {
				candidate.Content = &retrieval.Content{Text: *content, Format: "unknown", Freshness: "unknown"}
			}
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

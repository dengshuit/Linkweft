package retrieval

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"linkweft/internal/httpurl"
)

var (
	errCommandTimeout  = errors.New("command timeout")
	errProviderTimeout = errors.New("provider timeout")
)

func Search(
	ctx context.Context, request SearchRequest, options SearchOptions,
	names []string, explicit bool, registry map[string]SearchProvider, apiKeys []string,
) (Response, int) {
	operation := "search"
	response := NewResponse(&operation)
	if issue := route(&response, names, explicit, func(name string) (bool, bool) {
		entry, supported := registry[name]
		return supported, entry.Searcher != nil
	}); issue != nil {
		response.Outcome, response.Error = "error", issue
		return response, 2
	}

	commandCtx, stop := context.WithTimeoutCause(ctx, options.Timeout, errCommandTimeout)
	defer stop()
	if options.Strategy == "merge" {
		return mergeSearch(commandCtx, request, options.MaxConcurrency, registry, apiKeys, response)
	}
	return fallbackSearch(commandCtx, request, registry, apiKeys, response)
}

func fallbackSearch(ctx context.Context, request SearchRequest, registry map[string]SearchProvider, apiKeys []string, response Response) (Response, int) {
	failed := false
	for i := range response.Executions {
		execution := &response.Executions[i]
		if execution.Status == "skipped" {
			continue
		}
		if len(response.Results) > 0 {
			reason := "fallback_satisfied"
			execution.Status, execution.Reason = "not_started", &reason
			continue
		}
		if cause := context.Cause(ctx); cause != nil {
			_, issue := interruption(cause)
			execution.Status, execution.Reason = "not_started", &issue.Code
			failed = true
			continue
		}
		provider := registry[execution.Provider]
		start := time.Now()
		requestCtx, cancel := context.WithDeadlineCause(ctx, start.Add(provider.Timeout), errProviderTimeout)
		call := searchOne(requestCtx, start, request, execution.Provider, provider, apiKeys)
		cancel()
		*execution = call.execution
		response.Warnings = append(response.Warnings, call.warnings...)
		for _, candidate := range call.candidates {
			response.Results = append(response.Results, candidate.result(execution.Provider))
		}
		failed = failed || execution.Error != nil
	}
	return finishSearch(response, false, failed, context.Cause(ctx))
}

func finishSearch(response Response, merge, failed bool, cause error) (Response, int) {
	if cause != nil {
		if status, issue := interruption(cause); status == "cancelled" {
			response.Outcome, response.Error = status, issue
			return response, 130
		}
	}
	if len(response.Results) > 0 {
		response.Outcome = "success"
		if merge && failed {
			response.Outcome = "partial"
		}
		return response, 0
	}
	if failed {
		return fail(response, 1, "search_failed", "search failed; see the provider execution record")
	}
	response.Outcome = "empty"
	return response, 0
}

type searchCall struct {
	execution  Execution
	candidates []validatedCandidate
	warnings   []Warning
}

type validatedCandidate struct {
	Candidate
	key string
}

func (c validatedCandidate) result(provider string) Result {
	return Result{
		URL: c.URL, Title: c.Title, Snippet: c.Snippet, Content: c.Content,
		Sources: []Source{{Provider: provider, URL: c.URL, Rank: c.Rank}},
	}
}

func upstreamLimit(limit int, name string, maxResults int) (int, []Warning) {
	switch {
	case maxResults == 0:
		return 0, []Warning{{
			Code:     "upstream_limit_unavailable",
			Message:  fmt.Sprintf("%s does not accept a result limit; keeping up to %d valid results locally", name, limit),
			Provider: &name,
		}}
	case limit > maxResults:
		return maxResults, []Warning{{
			Code:     "upstream_limit_applied",
			Message:  fmt.Sprintf("requested up to %d results; %s accepts at most %d", limit, name, maxResults),
			Provider: &name,
		}}
	default:
		return limit, nil
	}
}

func searchOne(requestCtx context.Context, start time.Time, request SearchRequest, name string, provider SearchProvider, apiKeys []string) searchCall {
	upstreamRequest := request
	upstreamRequest.DisableSnippets = provider.DisableSnippets
	var call searchCall
	upstreamRequest.Limit, call.warnings = upstreamLimit(request.Limit, name, provider.MaxResults)
	var candidates []Candidate
	var issue *Error
	if requestCtx.Err() == nil {
		candidates, issue = provider.Search(requestCtx, upstreamRequest)
	}
	call.execution = Execution{Provider: name, EffectiveTimeoutMS: effectiveTimeoutMS(requestCtx, start)}
	if issue == nil && len(candidates) > 0 && requestCtx.Err() == nil {
		call.validate(candidates, request.Limit, apiKeys)
	}
	duration := time.Since(start).Milliseconds()
	call.execution.DurationMS = &duration
	if cause := context.Cause(requestCtx); cause != nil {
		interruptedExecution(&call.execution, cause)
		call.candidates = nil
		return call
	}
	execution := &call.execution
	if issue != nil {
		failedExecution(execution, issue)
	} else if len(candidates) == 0 {
		execution.Status = "empty"
	} else if len(call.candidates) == 0 {
		execution.Status = "error"
		execution.Error = &Error{Code: "invalid_response", Message: "upstream returned no valid result URLs"}
	} else {
		execution.Status = "success"
	}
	return call
}

func (call *searchCall) validate(candidates []Candidate, limit int, apiKeys []string) {
	name := call.execution.Provider
	dropped := 0
	for _, candidate := range candidates {
		address := httpurl.Parse(candidate.URL)
		if address == nil || candidate.Rank <= 0 ||
			containsEncodedAPIKey(candidate.URL, address.RawQuery, apiKeys) {
			dropped++
			continue
		}
		if len(call.candidates) < limit {
			candidate.Title = filteredText(candidate.Title, apiKeys)
			candidate.Snippet = filteredText(candidate.Snippet, apiKeys)
			candidate.Content = filteredContent(candidate.Content, name, apiKeys)
			call.candidates = append(call.candidates, validatedCandidate{Candidate: candidate, key: normalizedURL(candidate.URL, address)})
		}
	}
	if dropped > 0 {
		call.warnings = append(call.warnings, Warning{
			Code:     "invalid_results_dropped",
			Message:  fmt.Sprintf("discarded %d invalid search results", dropped),
			Provider: &name,
		})
	}
}

func containsEncodedAPIKey(address, query string, apiKeys []string) bool {
	if len(apiKeys) == 0 {
		return false
	}
	if containsAPIKey(address, apiKeys) || (strings.Contains(address, "%") && containsAPIKey(unescapeURL(address), apiKeys)) {
		return true
	}
	return strings.Contains(query, "+") && containsAPIKey(unescapeURL(strings.ReplaceAll(query, "+", " ")), apiKeys)
}

func unescapeURL(address string) string {
	if decoded, err := url.PathUnescape(address); err == nil {
		return decoded
	}
	// A malformed escape in a raw query can coexist with valid escapes elsewhere.
	var decoded strings.Builder
	decoded.Grow(len(address))
	for {
		prefix, tail, found := strings.Cut(address, "%")
		decoded.WriteString(prefix)
		if !found {
			return decoded.String()
		}
		if len(tail) >= 2 {
			if value, err := url.PathUnescape(address[len(prefix) : len(prefix)+3]); err == nil {
				decoded.WriteString(value)
				address = tail[2:]
				continue
			}
		}
		decoded.WriteByte('%')
		address = tail
	}
}

func filteredText(value *string, apiKeys []string) *string {
	if value != nil && (strings.TrimSpace(*value) == "" || containsEncodedAPIKey(*value, *value, apiKeys)) {
		return nil
	}
	return value
}

func filteredContent(value *Content, name string, apiKeys []string) *Content {
	if value == nil || strings.TrimSpace(value.Text) == "" || containsEncodedAPIKey(value.Text, value.Text, apiKeys) {
		return nil
	}
	content := *value
	content.Provider = name
	return &content
}

func containsAPIKey(value string, apiKeys []string) bool {
	for _, key := range apiKeys {
		if key != "" && strings.Contains(value, key) {
			return true
		}
	}
	return false
}

package retrieval

import (
	"context"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func ValidFetchURL(address string) bool {
	return parseFetchURL(address) != nil
}

func parseFetchURL(raw string) *url.URL {
	if !utf8.ValidString(raw) || strings.ContainsFunc(raw, unicode.IsSpace) {
		return nil
	}
	address := parseHTTPURL(raw)
	if address == nil {
		return nil
	}
	if _, err := url.PathUnescape(address.RawQuery); err != nil {
		return nil
	}
	return address
}

func Fetch(
	ctx context.Context, address string, options FetchOptions,
	names []string, explicit bool, registry map[string]FetchProvider, apiKeys []string,
) (Response, int) {
	operation := "fetch"
	response := NewResponse(&operation)
	parsed := parseFetchURL(address)
	if parsed == nil {
		return fail(response, 2, "invalid_argument", "fetch requires an absolute HTTP(S) URL with a hostname")
	}
	if containsEncodedAPIKey(address, parsed.RawQuery, apiKeys) {
		return fail(response, 2, "invalid_argument", "fetch URL must not contain an API key")
	}
	if issue := route(&response, names, explicit, func(name string) (bool, bool) {
		entry, supported := registry[name]
		return supported, entry.Fetcher != nil
	}); issue != nil {
		response.Outcome, response.Error = "error", issue
		return response, 2
	}

	commandCtx, stop := context.WithTimeoutCause(ctx, options.Timeout, errCommandTimeout)
	defer stop()
	for i := range response.Executions {
		execution := &response.Executions[i]
		if execution.Status == "skipped" {
			continue
		}
		if response.Document != nil {
			reason := "fallback_satisfied"
			execution.Status, execution.Reason = "not_started", &reason
			continue
		}
		response.Document = fetchOne(commandCtx, address, options.MaxChars, registry[execution.Provider], apiKeys, execution)
	}
	if cause := context.Cause(commandCtx); cause != nil {
		if status, issue := interruption(cause); status == "cancelled" {
			response.Outcome, response.Error = status, issue
			return response, 130
		}
	}
	if response.Document == nil {
		return fail(response, 1, "fetch_failed", "fetch failed; see the provider execution record")
	}
	response.Outcome = "success"
	return response, 0
}

func fetchOne(ctx context.Context, address string, maxChars int, provider FetchProvider, apiKeys []string, execution *Execution) *Document {
	if cause := context.Cause(ctx); cause != nil {
		_, issue := interruption(cause)
		execution.Status, execution.Reason = "not_started", &issue.Code
		return nil
	}
	start := time.Now()
	requestCtx, cancel := context.WithDeadlineCause(ctx, start.Add(provider.Timeout), errProviderTimeout)
	defer cancel()
	document, issue := provider.Fetch(requestCtx, address)
	if issue == nil && document != nil && requestCtx.Err() == nil {
		document, issue = prepareDocument(document, address, execution.Provider, maxChars, apiKeys)
	}
	duration := time.Since(start).Milliseconds()
	execution.DurationMS = &duration
	switch cause := context.Cause(requestCtx); {
	case cause != nil:
		interruptedExecution(execution, cause)
	case issue != nil:
		failedExecution(execution, issue)
	case document == nil:
		execution.Status = "empty"
	default:
		execution.Status = "success"
		return document
	}
	return nil
}

func prepareDocument(document *Document, requestedURL, provider string, maxChars int, apiKeys []string) (*Document, *Error) {
	if strings.TrimSpace(document.Content.Text) == "" {
		return nil, nil
	}
	if containsEncodedAPIKey(document.Content.Text, document.Content.Text, apiKeys) {
		return nil, &Error{Code: "invalid_response", Message: "provider content contains an API key"}
	}
	page := *document
	page.RequestedURL, page.Provider = requestedURL, provider
	page.ReturnedURL = safeDocumentURL(page.ReturnedURL, apiKeys)
	page.FinalURL = safeDocumentURL(page.FinalURL, apiKeys)
	page.Title = filteredText(page.Title, apiKeys)
	page.Content.Provider = provider
	page.Content.LocalTruncated = false
	for offset := range page.Content.Text {
		if maxChars == 0 {
			page.Content.Text = page.Content.Text[:offset]
			page.Content.LocalTruncated = true
			break
		}
		maxChars--
	}
	return &page, nil
}

func safeDocumentURL(address *string, apiKeys []string) *string {
	if address == nil {
		return nil
	}
	parsed := parseFetchURL(*address)
	if parsed == nil || containsEncodedAPIKey(*address, parsed.RawQuery, apiKeys) {
		return nil
	}
	return address
}

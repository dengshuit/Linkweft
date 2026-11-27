package retrieval

import (
	"cmp"
	"context"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

func mergeSearch(ctx context.Context, request SearchRequest, maxConcurrency int, registry map[string]SearchProvider, apiKeys []string, response Response) (Response, int) {
	type completion struct {
		index int
		call  searchCall
	}
	type runningRequest struct {
		start time.Time
		ctx   context.Context
	}
	count := len(response.Executions)
	completed := make(chan completion, count)
	started := make([]runningRequest, count)
	calls := make([]searchCall, count)
	// Serialize start and completion publication with the collector's final snapshot.
	var mu sync.Mutex
	launch := func(index int, name string) {
		provider := registry[name]
		go func() {
			mu.Lock()
			if ctx.Err() != nil {
				mu.Unlock()
				return
			}
			start := time.Now()
			requestCtx, cancel := context.WithDeadlineCause(ctx, start.Add(provider.Timeout), errProviderTimeout)
			started[index] = runningRequest{start: start, ctx: requestCtx}
			mu.Unlock()
			defer cancel()

			call := searchOne(requestCtx, start, request, name, provider, apiKeys)
			mu.Lock()
			if ctx.Err() == nil {
				completed <- completion{index, call}
			}
			mu.Unlock()
		}()
	}

	next, inFlight := 0, 0
collect:
	for {
		for next < count && inFlight < maxConcurrency && ctx.Err() == nil {
			execution := response.Executions[next]
			if execution.Status != "skipped" {
				launch(next, execution.Provider)
				inFlight++
			}
			next++
		}
		if inFlight == 0 {
			break
		}
		select {
		case done := <-completed:
			calls[done.index] = done.call
			inFlight--
		case <-ctx.Done():
			break collect
		}
	}

	mu.Lock()
	for len(completed) > 0 {
		done := <-completed
		calls[done.index] = done.call
	}
	starts := slices.Clone(started)
	mu.Unlock()

	collectionCause := context.Cause(ctx)
	end := time.Now()
	failed := false
	for i := range response.Executions {
		execution := &response.Executions[i]
		call := &calls[i]
		switch {
		case execution.Status == "skipped":
			continue
		case call.execution.Status != "":
			*execution = call.execution
		case starts[i].ctx == nil:
			_, issue := interruption(collectionCause)
			execution.Status, execution.Reason = "not_started", &issue.Code
		default:
			cause := collectionCause
			// The command can stop while an already timed-out adapter is still unwinding.
			if context.Cause(starts[i].ctx) == errProviderTimeout {
				cause = errProviderTimeout
			}
			interruptedExecution(execution, cause)
			duration := end.Sub(starts[i].start).Milliseconds()
			execution.DurationMS = &duration
			execution.EffectiveTimeoutMS = effectiveTimeoutMS(starts[i].ctx, starts[i].start)
			_, call.warnings = upstreamLimit(request.Limit, execution.Provider, registry[execution.Provider].MaxResults)
		}
		failed = failed || execution.Error != nil || execution.Status == "not_started"
		response.Warnings = append(response.Warnings, call.warnings...)
	}
	response.Results = fuse(calls, request.Limit, response.Results)
	return finishSearch(response, true, failed, context.Cause(ctx))
}

func normalizedURL(raw string, address *url.URL) string {
	// Use the original spelling outside the scheme and hostname, including URL delimiters.
	authority := raw[strings.IndexByte(raw, ':')+3:]
	var suffix string
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority, suffix = authority[:end], authority[end:]
	}
	hostStart := strings.LastIndexByte(authority, '@') + 1
	host := authority[hostStart:]
	if port := address.Port(); port != "" {
		number := strings.TrimLeft(port, "0")
		if (address.Scheme == "http" && number == "80") || (address.Scheme == "https" && number == "443") {
			host = strings.TrimSuffix(host, ":"+port)
		}
	}
	hostEnd := len(host)
	if strings.HasPrefix(host, "[") {
		// An IPv6 zone identifies an interface and can be case-sensitive.
		if zone := strings.Index(host, "%25"); zone >= 0 {
			hostEnd = zone
		}
	}
	return address.Scheme + "://" + authority[:hostStart] + strings.ToLower(host[:hostEnd]) + host[hostEnd:] + suffix
}

type fusedResult struct {
	result   Result
	score    big.Rat
	bestRank int
	priority int
	key      string
}

func fuse(calls []searchCall, limit int, results []Result) []Result {
	groups := make(map[string]*fusedResult)
	for priority, call := range calls {
		name := call.execution.Provider
		slices.SortStableFunc(call.candidates, func(a, b validatedCandidate) int { return cmp.Compare(a.Rank, b.Rank) })
		for _, candidate := range call.candidates {
			group := groups[candidate.key]
			if group == nil {
				group = &fusedResult{
					result: Result{URL: candidate.URL}, bestRank: candidate.Rank, priority: priority, key: candidate.key,
				}
				groups[candidate.key] = group
			}
			group.add(candidate, name)
		}
	}

	ranked := make([]*fusedResult, 0, len(groups))
	for _, group := range groups {
		ranked = append(ranked, group)
	}
	slices.SortFunc(ranked, compareFused)
	for _, group := range ranked[:min(limit, len(ranked))] {
		results = append(results, group.result)
	}
	return results
}

func (g *fusedResult) add(candidate validatedCandidate, name string) {
	sources := g.result.Sources
	if len(sources) == 0 || sources[len(sources)-1].Provider != name {
		g.result.Sources = append(sources, Source{Provider: name, URL: candidate.URL, Rank: candidate.Rank})
		g.bestRank = min(g.bestRank, candidate.Rank)
		var contribution big.Rat
		contribution.SetFrac(big.NewInt(1), new(big.Int).SetUint64(60+uint64(candidate.Rank)))
		g.score.Add(&g.score, &contribution)
	}
	if g.result.Title == nil {
		g.result.Title = candidate.Title
	}
	if g.result.Snippet == nil {
		g.result.Snippet = candidate.Snippet
	}
	if g.result.Content == nil {
		g.result.Content = candidate.Content
	}
}

func compareFused(a, b *fusedResult) int {
	if order := b.score.Cmp(&a.score); order != 0 {
		return order
	}
	if order := cmp.Compare(a.bestRank, b.bestRank); order != 0 {
		return order
	}
	if order := cmp.Compare(a.priority, b.priority); order != 0 {
		return order
	}
	return cmp.Compare(a.key, b.key)
}

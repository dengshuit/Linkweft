package retrieval

import (
	"context"
	"errors"
	"time"
)

var ErrUserCancelled = errors.New("user cancelled")

type Searcher interface {
	Search(context.Context, SearchRequest) ([]Candidate, *Error)
}

type SearchRequest struct {
	Query string
	Limit int // Zero when the upstream does not accept a limit.
}

type SearchOptions struct {
	Timeout        time.Duration
	Strategy       string
	MaxConcurrency int
}

type Candidate struct {
	URL     string
	Title   *string
	Snippet *string
	Content *Content
	Rank    int
}

type SearchProvider struct {
	Searcher
	Timeout    time.Duration
	MaxResults int // Zero when the upstream does not accept a limit.
}

type Fetcher interface {
	Fetch(context.Context, string) (*Document, *Error)
}

type FetchProvider struct {
	Fetcher
	Timeout time.Duration
}

type FetchOptions struct {
	Timeout  time.Duration
	MaxChars int
}

type Response struct {
	SchemaVersion int         `json:"schema_version"`
	Operation     *string     `json:"operation"`
	Outcome       string      `json:"outcome"`
	Results       []Result    `json:"results"`
	Document      *Document   `json:"document"`
	Executions    []Execution `json:"executions"`
	Warnings      []Warning   `json:"warnings"`
	Error         *Error      `json:"error"`
}

type Result struct {
	URL     string   `json:"url"`
	Title   *string  `json:"title"`
	Snippet *string  `json:"snippet"`
	Content *Content `json:"content"`
	Sources []Source `json:"sources"`
}

type Document struct {
	RequestedURL string  `json:"requested_url"`
	ReturnedURL  *string `json:"returned_url"`
	FinalURL     *string `json:"final_url"`
	Provider     string  `json:"provider"`
	Title        *string `json:"title"`
	Content      Content `json:"content"`
}

type Content struct {
	Text              string `json:"text"`
	Provider          string `json:"provider"`
	Format            string `json:"format"`
	LocalTruncated    bool   `json:"local_truncated"`
	UpstreamTruncated *bool  `json:"upstream_truncated"`
	Freshness         string `json:"freshness"`
}

type Source struct {
	Provider string `json:"provider"`
	URL      string `json:"url"`
	Rank     int    `json:"rank"`
}

type Execution struct {
	Provider   string  `json:"provider"`
	Status     string  `json:"status"`
	DurationMS *int64  `json:"duration_ms"`
	Reason     *string `json:"reason"`
	Error      *Error  `json:"error"`
}

type Warning struct {
	Code     string  `json:"code"`
	Message  string  `json:"message"`
	Provider *string `json:"provider"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

func NewResponse(operation *string) Response {
	return Response{
		SchemaVersion: 1,
		Operation:     operation,
		Results:       []Result{},
		Executions:    []Execution{},
		Warnings:      []Warning{},
	}
}

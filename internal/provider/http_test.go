package provider

import (
	"testing"
	"time"
)

func TestRetryAfterMS(t *testing.T) {
	now := time.Date(2026, time.November, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  int64
		valid bool
	}{
		{"7", 7000, true},
		{"0", 0, true},
		{" 7 ", 7000, true},
		{"Fri, 27 Nov 2026 12:00:07 GMT", 7000, true},
		{"Fri, 27 Nov 2026 11:59:59 GMT", 0, true},
		{"", 0, false},
		{"invalid", 0, false},
		{"-1", 0, false},
		{"+1", 0, false},
		{"1.5", 0, false},
		{"9223372036854776", 0, false},
		{"999999999999999999999999", 0, false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			got := retryAfterMS(tc.value, now)
			if !tc.valid {
				if got != nil {
					t.Fatalf("retryAfterMS(%q) = %d, want nil", tc.value, *got)
				}
			} else if got == nil || *got != tc.want {
				t.Fatalf("retryAfterMS(%q) = %v, want %d", tc.value, got, tc.want)
			}
		})
	}
}

func TestOperationURLRootPath(t *testing.T) {
	const defaultBase = "https://default.example"
	for _, tc := range []struct{ base, want string }{
		{"", defaultBase + "/"},
		{"https://gateway.example", "https://gateway.example/"},
		{"https://gateway.example/", "https://gateway.example/"},
		{"https://gateway.example/tenant", "https://gateway.example/tenant/"},
		{"https://gateway.example/tenant/", "https://gateway.example/tenant/"},
	} {
		if got := operationURL(tc.base, defaultBase, "/"); got != tc.want {
			t.Errorf("operationURL(%q, %q, %q) = %q, want %q", tc.base, defaultBase, "/", got, tc.want)
		}
	}
}

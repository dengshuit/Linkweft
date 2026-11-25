package provider

import "testing"

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

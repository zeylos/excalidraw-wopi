package app

import "testing"

// TestIsLoopbackHost covers the loopback check directly: only localhost
// and a loopback IP literal must pass.
func TestIsLoopbackHost(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"http://localhost:8080", true},
		{"http://LOCALHOST:8080", true},
		{"http://127.0.0.1:8080", true},
		{"http://127.5.6.7:8080", true},
		{"http://[::1]:8080", true},
		{"https://excalidraw.example.org", false},
		{"http://10.0.0.5:8080", false},
		{"http://0.0.0.0:8080", false},
		{"not a url", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := isLoopbackHost(tt.url); got != tt.want {
				t.Errorf("isLoopbackHost(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

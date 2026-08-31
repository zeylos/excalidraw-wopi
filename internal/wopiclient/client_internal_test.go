package wopiclient

import (
	"errors"
	"net/http"
	"testing"
)

// TestNewNilClientGetsBoundedTimeoutAndNoRedirects checks that a Client
// built with a nil httpClient must not fall back to http.DefaultClient,
// which has neither a timeout nor a redirect policy.
// This test lives in package wopiclient itself, not wopiclient_test,
// since it inspects the unexported http field directly rather than
// dialing a real connection.
func TestNewNilClientGetsBoundedTimeoutAndNoRedirects(t *testing.T) {
	c := New(nil, nil, nil)

	if c.http == http.DefaultClient {
		t.Fatal("a nil httpClient must not fall back to http.DefaultClient")
	}
	if c.http.Timeout <= 0 {
		t.Errorf("http.Timeout = %v, want a positive bound", c.http.Timeout)
	}
	if c.http.CheckRedirect == nil {
		t.Fatal("CheckRedirect must be set to refuse redirects")
	}
	if err := c.http.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect(...) = %v, want http.ErrUseLastResponse", err)
	}
}

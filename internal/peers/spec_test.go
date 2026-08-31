package peers

import "testing"

func TestNewParsesValidDNSSpec(t *testing.T) {
	c, err := New(Config{
		DNS:  "excalidraw-wopi-headless:8080",
		Self: "http://10.0.0.1:8080",
	})
	if err != nil {
		t.Fatalf("New() returned an error: %v", err)
	}
	if !c.Enabled() {
		t.Fatal("Enabled() = false, want true")
	}
	if c.dnsHost != "excalidraw-wopi-headless" {
		t.Errorf("dnsHost = %q, want %q", c.dnsHost, "excalidraw-wopi-headless")
	}
	if c.dnsPort != "8080" {
		t.Errorf("dnsPort = %q, want %q", c.dnsPort, "8080")
	}
}

func TestNewRejectsDNSSpecWithBadPort(t *testing.T) {
	_, err := New(Config{
		DNS:  "excalidraw-wopi-headless:notaport",
		Self: "http://10.0.0.1:8080",
	})
	if err == nil {
		t.Error("New() must reject a dns spec with a non-numeric port")
	}
}

func TestNewRejectsDNSSpecWithBadSelfURL(t *testing.T) {
	_, err := New(Config{
		DNS:  "excalidraw-wopi-headless:8080",
		Self: "not-a-url",
	})
	if err == nil {
		t.Error("New() must reject a self URL with no http(s) scheme")
	}
}

func TestNewSucceedsForDisabledClusterRegardlessOfSelf(t *testing.T) {
	c, err := New(Config{})
	if err != nil {
		t.Fatalf("New() returned an error: %v", err)
	}
	if c.Enabled() {
		t.Error("Enabled() = true for an empty spec, want false")
	}
}

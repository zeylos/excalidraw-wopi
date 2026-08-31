package discovery

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeylos/excalidraw-wopi/internal/config"
	"github.com/zeylos/excalidraw-wopi/internal/proof"
)

func testKeySet(t *testing.T) *proof.KeySet {
	t.Helper()
	cfg := config.Config{ProofKeyPath: filepath.Join(t.TempDir(), "proof-key.pem")}
	ks, err := proof.Load(cfg)
	if err != nil {
		t.Fatalf("proof.Load(): %v", err)
	}
	return ks
}

// parsedDiscovery mirrors the subset of the XML Drive's
// configure_wopi.py reads: a net-zone found anywhere in the tree, an
// app/action pair, and a proof-key with all six attributes. ProofKey is
// tagged on the top-level struct, so it matches only a <proof-key> that
// is a direct child of <wopi-discovery>: a test built against a struct
// like this would silently pass against a net-zone-nested placement too,
// so TestProofKeyIsDirectChildOfRoot below checks the raw XML text as
// well.
type parsedDiscovery struct {
	XMLName xml.Name `xml:"wopi-discovery"`
	NetZone struct {
		Name string `xml:"name,attr"`
		Apps []struct {
			Name    string `xml:"name,attr"`
			Actions []struct {
				Name   string `xml:"name,attr"`
				Ext    string `xml:"ext,attr"`
				URLSrc string `xml:"urlsrc,attr"`
			} `xml:"action"`
		} `xml:"app"`
	} `xml:"net-zone"`
	ProofKey struct {
		Value       string `xml:"value,attr"`
		Modulus     string `xml:"modulus,attr"`
		Exponent    string `xml:"exponent,attr"`
		OldValue    string `xml:"oldvalue,attr"`
		OldModulus  string `xml:"oldmodulus,attr"`
		OldExponent string `xml:"oldexponent,attr"`
	} `xml:"proof-key"`
}

func TestRenderStructure(t *testing.T) {
	ks := testKeySet(t)

	body, err := Render("https://excalidraw.example.org", ks)
	if err != nil {
		t.Fatalf("Render(): %v", err)
	}

	if !strings.HasPrefix(string(body), xml.Header) {
		t.Error("Render() output must start with an XML declaration")
	}

	var doc parsedDiscovery
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("Unmarshal(Render() output): %v", err)
	}

	if doc.NetZone.Name != "external-https" {
		t.Errorf("net-zone name = %q, want %q", doc.NetZone.Name, "external-https")
	}

	if len(doc.NetZone.Apps) != 1 || doc.NetZone.Apps[0].Name != "excalidraw" {
		t.Fatalf("apps = %+v, want exactly one app named excalidraw", doc.NetZone.Apps)
	}
	actions := doc.NetZone.Apps[0].Actions
	if len(actions) != 1 {
		t.Fatalf("actions = %+v, want exactly one action", actions)
	}
	action := actions[0]
	if action.Name != "edit" {
		t.Errorf("action name = %q, want %q", action.Name, "edit")
	}
	if action.Ext != "excalidraw" {
		t.Errorf("action ext = %q, want lowercase %q", action.Ext, "excalidraw")
	}
	wantURLSrc := "https://excalidraw.example.org/launch"
	if action.URLSrc != wantURLSrc {
		t.Errorf("action urlsrc = %q, want %q", action.URLSrc, wantURLSrc)
	}
	if strings.Contains(action.URLSrc, "?") {
		t.Error("urlsrc must carry no query string; Drive discards and rebuilds it")
	}

	pk := doc.ProofKey
	for name, v := range map[string]string{
		"value": pk.Value, "modulus": pk.Modulus, "exponent": pk.Exponent,
		"oldvalue": pk.OldValue, "oldmodulus": pk.OldModulus, "oldexponent": pk.OldExponent,
	} {
		if v == "" {
			t.Errorf("proof-key attribute %q must not be empty", name)
		}
	}
}

func TestHandlerServesXML(t *testing.T) {
	ks := testKeySet(t)
	handler := Handler("https://excalidraw.example.org", ks)

	req := httptest.NewRequest(http.MethodGet, "/hosting/discovery", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/xml" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/xml")
	}

	var doc parsedDiscovery
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("Unmarshal(handler response body): %v", err)
	}
	if doc.NetZone.Name != "external-https" {
		t.Errorf("net-zone name = %q, want %q", doc.NetZone.Name, "external-https")
	}
}

// TestNetZoneNameFollowsPublicURLScheme checks that the net-zone name
// reflects whether PUBLIC_URL is http or https, not a fixed value.
func TestNetZoneNameFollowsPublicURLScheme(t *testing.T) {
	ks := testKeySet(t)

	httpBody, err := Render("http://excalidraw.example.org", ks)
	if err != nil {
		t.Fatalf("Render(http): %v", err)
	}
	var httpDoc parsedDiscovery
	if err := xml.Unmarshal(httpBody, &httpDoc); err != nil {
		t.Fatalf("Unmarshal(http): %v", err)
	}
	if httpDoc.NetZone.Name != "external-http" {
		t.Errorf("net-zone name for an http PUBLIC_URL = %q, want %q", httpDoc.NetZone.Name, "external-http")
	}

	httpsBody, err := Render("https://excalidraw.example.org", ks)
	if err != nil {
		t.Fatalf("Render(https): %v", err)
	}
	var httpsDoc parsedDiscovery
	if err := xml.Unmarshal(httpsBody, &httpsDoc); err != nil {
		t.Fatalf("Unmarshal(https): %v", err)
	}
	if httpsDoc.NetZone.Name != "external-https" {
		t.Errorf("net-zone name for an https PUBLIC_URL = %q, want %q", httpsDoc.NetZone.Name, "external-https")
	}
}

// TestProofKeyIsDirectChildOfRoot checks the raw XML text, so a
// struct-tag mistake in parsedDiscovery cannot mask a proof key nested
// under net-zone.
func TestProofKeyIsDirectChildOfRoot(t *testing.T) {
	ks := testKeySet(t)

	body, err := Render("https://excalidraw.example.org", ks)
	if err != nil {
		t.Fatalf("Render(): %v", err)
	}

	netZoneClose := strings.Index(string(body), "</net-zone>")
	proofKeyOpen := strings.Index(string(body), "<proof-key")
	if netZoneClose == -1 || proofKeyOpen == -1 {
		t.Fatalf("body is missing </net-zone> or <proof-key>:\n%s", body)
	}
	if proofKeyOpen < netZoneClose {
		t.Errorf("<proof-key> appears before </net-zone> closes: it must sit outside net-zone, as a sibling under wopi-discovery")
	}
}

type failingKeys struct{}

func (failingKeys) CurrentPublicParts() (proof.PublicKeyParts, error) {
	return proof.PublicKeyParts{}, errTest
}

func (failingKeys) OldPublicParts() (proof.PublicKeyParts, error) {
	return proof.PublicKeyParts{}, errTest
}

var errTest = errTestType{}

type errTestType struct{}

func (errTestType) Error() string { return "boom" }

func TestHandlerReturns500OnRenderError(t *testing.T) {
	handler := Handler("https://excalidraw.example.org", failingKeys{})

	req := httptest.NewRequest(http.MethodGet, "/hosting/discovery", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

// Package discovery renders the WOPI discovery XML that Drive fetches on
// its nightly configuration task and serves it over HTTP.
package discovery

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"

	"github.com/zeylos/excalidraw-wopi/internal/proof"
)

// PublicKeySet is the subset of proof.KeySet discovery needs: the public
// parts of the current and the old proof key.
type PublicKeySet interface {
	CurrentPublicParts() (proof.PublicKeyParts, error)
	OldPublicParts() (proof.PublicKeyParts, error)
}

// discoveryXML places proof-key as a direct child of wopi-discovery, not
// nested inside net-zone: that is where MS-WOPI and both reference office
// suites put it.
type discoveryXML struct {
	XMLName  xml.Name  `xml:"wopi-discovery"`
	NetZone  netZone   `xml:"net-zone"`
	ProofKey *proofKey `xml:"proof-key"`
}

type netZone struct {
	Name string `xml:"name,attr"`
	Apps []app  `xml:"app"`
}

type app struct {
	Name   string `xml:"name,attr"`
	Action action `xml:"action"`
}

type action struct {
	Name   string `xml:"name,attr"`
	Ext    string `xml:"ext,attr"`
	URLSrc string `xml:"urlsrc,attr"`
}

// proofKey mirrors the WOPI <proof-key> element. The WOPI discovery
// schema does not require the six attributes, but Drive does: a missing
// one crashes its config task, so every field here is always populated.
type proofKey struct {
	Value       string `xml:"value,attr"`
	Modulus     string `xml:"modulus,attr"`
	Exponent    string `xml:"exponent,attr"`
	OldValue    string `xml:"oldvalue,attr"`
	OldModulus  string `xml:"oldmodulus,attr"`
	OldExponent string `xml:"oldexponent,attr"`
}

// Render builds the discovery XML for publicURL (the service's own
// externally reachable base URL) and keys (the proof key set to publish).
func Render(publicURL string, keys PublicKeySet) ([]byte, error) {
	current, err := keys.CurrentPublicParts()
	if err != nil {
		return nil, fmt.Errorf("current proof key parts: %w", err)
	}
	old, err := keys.OldPublicParts()
	if err != nil {
		return nil, fmt.Errorf("old proof key parts: %w", err)
	}

	doc := discoveryXML{
		NetZone: netZone{
			Name: netZoneName(publicURL),
			Apps: []app{
				{
					Name: "excalidraw",
					Action: action{
						Name:   "edit",
						Ext:    "excalidraw",
						URLSrc: publicURL + "/launch",
					},
				},
			},
		},
		ProofKey: &proofKey{
			Value:       current.Value,
			Modulus:     current.Modulus,
			Exponent:    current.Exponent,
			OldValue:    old.Value,
			OldModulus:  old.Modulus,
			OldExponent: old.Exponent,
		},
	}

	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal discovery xml: %w", err)
	}
	return append([]byte(xml.Header), body...), nil
}

// netZoneName derives the WOPI net-zone name from publicURL's scheme.
func netZoneName(publicURL string) string {
	if u, err := url.Parse(publicURL); err == nil && u.Scheme == "https" {
		return "external-https"
	}
	return "external-http"
}

// Handler returns the GET /hosting/discovery handler. publicURL is the
// service's own externally reachable base URL, and keys is the proof key
// set to publish.
func Handler(publicURL string, keys PublicKeySet) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		body, err := Render(publicURL, keys)
		if err != nil {
			http.Error(w, "failed to render discovery xml", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"testing"
)

// AgentToken finds an outside agent's token on a page.
var AgentToken = regexp.MustCompile(`gna_[A-Za-z0-9_-]{20,}`)

// Program calls a network's agent API as an outside agent's Program would:
// no cookies, no browser, just its token.
func (w *World) Program(network, token, method, path string, body any) Response {
	w.T.Helper()
	var in io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		in = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, w.Server.URL+path, in)
	req.Host = network + "." + BaseDomain
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.T.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return Response{Status: resp.StatusCode, Body: string(raw), Header: resp.Header}
}

// HireOutside hires rex as an outside agent from the console and returns
// the page that follows, and the token on it.
func HireOutside(t *testing.T, w *World) (*Browser, Response, string) {
	t.Helper()
	ada := w.Browser("acme").SignedIn("ada")
	ada.Get("/")
	r := ada.Post("/staff", url.Values{"nick": {"rex"}, "role": {"Release manager"}, "team": {"Engineering"}, "platform": {"outside"}})
	token := AgentToken.FindString(r.Body)
	if r.Status != http.StatusOK || token == "" {
		t.Fatalf("hiring rex to run outside: %d, token %q\n%s", r.Status, token, r.Text())
	}
	return ada, r, token
}

// AsNetwork sends requests to a network's address, with an agent's token.
type AsNetwork struct{ Host, Token string }

func (a AsNetwork) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Host = a.Host
	req.Header.Set("Authorization", "Bearer "+a.Token)
	return http.DefaultTransport.RoundTrip(req)
}

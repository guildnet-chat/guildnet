// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// Agents that run outside GuildNet can call models through GuildNet's model
// gateway, at /models/ on their network's address, in the hosted edition:
// an extension answers those calls, and says whether it's on for a network.
// The open console has no gateway: an outside agent brings its own keys.

func TestAnExtensionAnswersAgentsModelCalls(t *testing.T) {
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, func(o *Setup) {
		o.Extension = platform.ExtensionFunc(func(*platform.Core) platform.Hooks {
			return platform.Hooks{Models: &platform.ModelGateway{
				Serve: func(w http.ResponseWriter, r *http.Request, n platform.Network) {
					io.WriteString(w, "models for "+n.Name+" at "+r.URL.Path)
				},
				On: func(r *http.Request, n platform.Network) bool { return n.Name == "acme" },
			}}
		})
	})
	ada, hired, token := HireOutside(t, w)

	if r := w.Program("acme", token, "GET", "/models/v1/models", nil); r.Status != http.StatusOK || r.Body != "models for acme at /models/v1/models" {
		t.Errorf("a model call: %d %q", r.Status, r.Body)
	}
	if !strings.Contains(hired.Body, "/models/v1") {
		t.Error("the new agent's token page doesn't say where its models are")
	}
	page := ada.Get("/staff/rex").Text()
	if !strings.Contains(page, "model gateway") || !strings.Contains(page, "/models/v1") || strings.Contains(page, "isn't on") {
		t.Errorf("rex's page doesn't say the gateway is on, and where:\n%s", page)
	}
}

func TestTheOpenConsoleHasNoModelGateway(t *testing.T) {
	w := NewWorld(t)
	ada, hired, token := HireOutside(t, w)

	r := w.Program("acme", token, "POST", "/models/v1/chat/completions", AskModel(nil))
	if r.Status != http.StatusNotFound {
		t.Errorf("a model call: %d %s, want 404", r.Status, r.Body)
	}
	if strings.Contains(hired.Text(), "model gateway") || strings.Contains(hired.Body, "/models/v1") {
		t.Errorf("the token page offers a model gateway:\n%s", hired.Text())
	}
	if page := ada.Get("/staff/rex").Text(); strings.Contains(page, "model gateway") {
		t.Errorf("rex's page offers a model gateway:\n%s", page)
	}
}

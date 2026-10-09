// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/models"
)

// NewFakeModelProviders answers the providers' key checks: any key with
// "refused" in it is refused.
func NewFakeModelProviders(t *testing.T) map[models.Provider]string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Api-Key") + r.Header.Get("Authorization")
		if key == "" || strings.Contains(key, "refused") {
			http.Error(w, `{"error":"invalid key"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)
	return map[models.Provider]string{
		models.Anthropic: srv.URL + "/", models.OpenAI: srv.URL + "/v1/", models.Google: srv.URL + "/v1beta/openai/", models.OpenRouter: srv.URL + "/api/v1/",
	}
}

// AskModel is a model call's body, as an agent outside sends one, with
// extra in it.
func AskModel(extra map[string]any) map[string]any {
	b := map[string]any{"model": "default", "messages": []map[string]string{{"role": "user", "content": "ship it?"}}}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

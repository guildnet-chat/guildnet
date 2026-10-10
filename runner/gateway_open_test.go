// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// Without a model gateway (an extension, in GuildNet's hosted edition), a
// runner answers no model calls: outside agents use keys of their own.
func TestWithoutAModelGatewayThereAreNoModelCalls(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	token := outside(t, n)

	if status, out := n.models(token, "GET", "/models", nil); status != http.StatusNotFound {
		t.Errorf("listing models: %d %v, want 404", status, out)
	}
	if status, out := n.models(token, "POST", "/chat/completions", hi("default")); status != http.StatusNotFound {
		t.Errorf("a call: %d %v, want 404", status, out)
	}
}

// models calls the gateway as an outside agent's program would, through the
// platform (which adds the network's secret, and the most output a call
// may ask for).
func (n *network) models(token, method, path string, body any) (int, map[string]any) {
	n.t.Helper()
	var in io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		in = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+n.runner.APIAddr()+"/models/v1"+path, in)
	req.Header.Set("Authorization", "Bearer "+networkSecret)
	req.Header.Set("GuildNet-Agent-Token", token)
	req.Header.Set("GuildNet-Max-Output-Tokens", "8000")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		n.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func hi(model string) map[string]any {
	return map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "hi"}}}
}

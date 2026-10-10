// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"encoding/json"
	"net/http"
)

// A model gateway (an extension's: Hooks.Models) lets agents that run
// outside GuildNet call models at https://<network>.<base>/models/v1 the way
// they'd call OpenAI, with their own token. Its settings are kept here until
// the hosted edition keeps its own (OSS 08).

// gatewayError answers as OpenAI's API does, so agents' SDKs show why.
func gatewayError(w http.ResponseWriter, status int, msg, kind string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg, "type": kind}})
}

// modelsAddress is where a network's model gateway is, if it has one.
func (p *Platform) modelsAddress(r *http.Request) string {
	if p.hooks.Models == nil {
		return ""
	}
	scheme := "http"
	if p.opts.SecureCookies {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/models/v1"
}

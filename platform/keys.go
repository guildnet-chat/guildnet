// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/models"
	"github.com/guildnet-chat/guildnet/monitor"
)

// Model providers' keys: what each looks like, and asking the provider
// whether it takes one, so a bad key is found when it's given, not when an
// agent is first woken. A key is only ever sent to its own provider, at its
// fixed address.

var keyShapes = map[models.Provider]struct {
	re   *regexp.Regexp
	hint string
}{
	models.Anthropic: {regexp.MustCompile(`^sk-ant-[A-Za-z0-9_-]{10,240}$`), "they start with sk-ant-"},
	models.OpenAI:    {regexp.MustCompile(`^sk-[A-Za-z0-9_-]{16,240}$`), "they start with sk-"},
	// AI Studio's keys start "AQ." (since May 2026), older ones "AIza".
	models.Google:     {regexp.MustCompile(`^(AQ\.[A-Za-z0-9_-][A-Za-z0-9._-]{16,500}|AIza[A-Za-z0-9_-]{16,240})$`), "they start with AQ. or AIza"},
	models.OpenRouter: {regexp.MustCompile(`^sk-or-[A-Za-z0-9_-]{16,240}$`), "they start with sk-or-"},
}

// providerNamed is the provider a form names, if it's one.
func providerNamed(name string) (models.Provider, bool) {
	for _, p := range models.Providers {
		if strings.EqualFold(name, string(p)) {
			return p, true
		}
	}
	return "", false
}

// keyProblem says what's wrong with key as p's, in words for the person.
func keyProblem(p models.Provider, key string) string {
	shape, ok := keyShapes[p]
	if !ok {
		return "Pick a model provider."
	}
	if !shape.re.MatchString(key) {
		return fmt.Sprintf("That doesn't look like a %s API key (%s).", p.Title(), shape.hint)
	}
	return ""
}

// keyRefused asks p whether it takes key. Only a refusal counts: if the
// provider can't be reached, the key is taken, and a bad one shows when an
// agent is woken.
func (p *Platform) keyRefused(ctx context.Context, prov models.Provider, key string) bool {
	base := p.opts.ModelProviders[prov]
	if base == "" {
		base = prov.BaseURL()
	}
	var url string
	switch prov {
	case models.Anthropic:
		if base == "" {
			base = "https://api.anthropic.com/"
		}
		url = base + "v1/models"
	case models.OpenRouter:
		url = base + "key" // its models are public; this one needs the key
	default:
		url = base + "models"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false
	}
	if prov == models.Anthropic {
		req.Header.Set("X-Api-Key", key)
		req.Header.Set("Anthropic-Version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		monitor.Problem(p.opts.Logger, "checking a %s key: %v", prov.Title(), err)
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
}

// checkKey says what's wrong with key as prov's (its shape, or the provider
// refusing it), in words for the person, or "".
func (p *Platform) checkKey(ctx context.Context, prov models.Provider, key string) string {
	if msg := keyProblem(prov, key); msg != "" {
		return msg
	}
	if p.keyRefused(ctx, prov, key) {
		return fmt.Sprintf("%s didn't accept that key. Check it's copied whole and still active.", prov.Title())
	}
	return ""
}

// keyEnd is the end of a key, which is all that's ever shown of it.
func keyEnd(key string) string {
	if len(key) <= 4 {
		return ""
	}
	return key[len(key)-4:]
}

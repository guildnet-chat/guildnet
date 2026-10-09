// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/internal/brain"
	"github.com/guildnet-chat/guildnet/templates"
)

// A self-hosted network's runner (GUILDNET_SETTINGS_FROM_BROKER=on) gets
// its settings from the console's broker once first-run setup has made the
// network: which template it starts from, who owns it, the company and its
// timezone. Until then it waits.

// networkSettings are what the broker gives.
type networkSettings struct {
	Template string `json:"template"`
	Owner    string `json:"owner"`
	Company  string `json:"company"`
	Timezone string `json:"timezone"`
}

// errNotSetUp is the broker saying first-run setup hasn't made the network
// yet.
var errNotSetUp = errors.New("the network isn't set up yet")

// fetchSettings asks the broker for the network's settings.
func fetchSettings(ctx context.Context, b *brain.Broker) (networkSettings, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(b.URL, "/")+"/settings", nil)
	if err != nil {
		return networkSettings{}, err
	}
	req.Header.Set("GuildNet-Network", b.Network)
	req.Header.Set("Authorization", "Bearer "+b.Secret)
	client := b.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return networkSettings{}, fmt.Errorf("asking the broker for the network's settings: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return networkSettings{}, errNotSetUp
	default:
		return networkSettings{}, fmt.Errorf("the broker didn't give the network's settings: %s", resp.Status)
	}
	var s networkSettings
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&s); err != nil || s.Template == "" || s.Owner == "" {
		return networkSettings{}, errors.New("the broker's answer wasn't the network's settings")
	}
	return s, nil
}

// waitForSettings asks the broker every so often until it gives the
// network's settings, saying once that it's waiting (and why, when it's
// something else).
func waitForSettings(ctx context.Context, b *brain.Broker, every time.Duration, logger *log.Logger) (networkSettings, error) {
	said := ""
	for {
		s, err := fetchSettings(ctx, b)
		if err == nil {
			return s, nil
		}
		if msg := err.Error(); msg != said {
			if errors.Is(err, errNotSetUp) {
				logger.Print("waiting for first-run setup: open the console's address to set the network up")
			} else {
				logger.Printf("waiting for the network's settings: %v", err)
			}
			said = msg
		}
		select {
		case <-ctx.Done():
			return networkSettings{}, ctx.Err()
		case <-time.After(every):
		}
	}
}

// configPath is the template's runner config, under dir.
func (s networkSettings) configPath(dir string) (string, error) {
	if !templates.Known(s.Template) {
		return "", fmt.Errorf("there's no template %q", s.Template)
	}
	return filepath.Join(dir, s.Template, "guildnet.yaml"), nil
}

// env is the settings as the runner's config reads them from the
// environment.
func (s networkSettings) env() map[string]string {
	return map[string]string{"GUILDNET_OWNER": s.Owner, "GUILDNET_COMPANY": s.Company, "GUILDNET_TIMEZONE": s.Timezone}
}

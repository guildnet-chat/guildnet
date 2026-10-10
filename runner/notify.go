// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/monitor"
)

// BrokerNews tells the platform's broker, as the network, with its secret.
type BrokerNews struct {
	URL, Network, Secret string
	// HTTP is how the broker is reached (one trusting GuildNet's own CA,
	// inside the VM); nil means a plain client.
	HTTP *http.Client
}

// Notify sends e to the broker.
func (b BrokerNews) Notify(ctx context.Context, e Event) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(b.URL, "/")+"/events", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("GuildNet-Network", b.Network)
	req.Header.Set("Authorization", "Bearer "+b.Secret)
	req.Header.Set("Content-Type", "application/json")
	client := b.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("telling the broker: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the broker didn't take it: %s", resp.Status)
	}
	return nil
}

// News for the platform, which emails whoever wants to hear it: who an
// approval is waiting on, and the network reaching its daily spending cap.
// It's only news: sent in the background, never waited on.

// Event is one piece of news.
type Event struct {
	Type     string         `json:"type"` // "approval" or "spend_cap"
	Approval *ApprovalEvent `json:"approval,omitempty"`
	Spend    *SpendEvent    `json:"spend,omitempty"`
}

// ApprovalEvent says an approval is waiting on Approver: just raised, or
// moved on from MovedFrom, who didn't answer.
type ApprovalEvent struct {
	ID        int64     `json:"id"`
	Agent     string    `json:"agent"`
	Summary   string    `json:"summary"`
	Where     string    `json:"where"` // a channel, or "a DM"
	Asker     string    `json:"asker"`
	Approver  string    `json:"approver"`
	MovedFrom string    `json:"moved_from,omitempty"`
	Expires   time.Time `json:"expires"`
	OwnWords  bool      `json:"own_words,omitempty"` // the summary is an outside agent's own words
}

// SpendEvent says the network spent its daily cap.
type SpendEvent struct {
	Spent float64 `json:"spent"`
	Cap   float64 `json:"cap"`
}

// Notifier tells the platform.
type Notifier interface {
	Notify(ctx context.Context, e Event) error
}

// NotifyWith has the runner tell n its news; nil for nobody.
func (r *Runner) NotifyWith(n Notifier) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notifier = n
}

// tellNews sends e in the background. Not to be called holding mu.
func (r *Runner) tellNews(e Event) {
	r.mu.Lock()
	n := r.notifier
	r.mu.Unlock()
	if n == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := n.Notify(ctx, e); err != nil {
			monitor.Problem(r.logger, "telling the platform (%s): %v", e.Type, err)
		}
	}()
}

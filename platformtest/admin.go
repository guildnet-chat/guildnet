// SPDX-License-Identifier: AGPL-3.0-only

package platformtest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// AdminEmail is who signs in as an admin (World.Admin): an extension's, if
// it says they are one (Hooks.IsAdmin).
const AdminEmail = "avery@guildnet.test"

// Admin is GuildNet's Admin, signed in with Google. Admins needn't be on the
// beta list.
func (w *World) Admin() *Browser {
	w.T.Helper()
	b := w.Home()
	if r := b.SignInWithGoogle(AdminEmail); r.Status != http.StatusSeeOther {
		w.T.Fatalf("signing in as the admin: %d %s", r.Status, r.Body)
	}
	return b
}

// CreateNetwork has email (on the beta list) create a network called name
// and waits until it's ready.
func (w *World) CreateNetwork(email, name string) *Browser {
	w.T.Helper()
	b := w.OnHome(email)
	b.Get("/networks/new")
	if r := b.Post("/networks", CreateForm(name)); r.Status != http.StatusSeeOther {
		w.T.Fatalf("creating %s: %d %s", name, r.Status, r.Text())
	}
	if r := b.WaitReady(name); !strings.Contains(r.Text(), "is ready") {
		w.T.Fatalf("%s didn't come up: %s", name, r.Text())
	}
	return b
}

// RowFor is the table row on page that mentions what.
func RowFor(t *testing.T, page, what string) string {
	t.Helper()
	for _, row := range regexp.MustCompile(`(?s)<tr.*?</tr>`).FindAllString(page, -1) {
		if strings.Contains(row, what) {
			return regexp.MustCompile(`<[^>]+>`).ReplaceAllString(row, " ")
		}
	}
	t.Fatalf("no row for %s on:\n%s", what, page)
	return ""
}

// Created has email create network from the team template and open its
// console as ada, the owner. It returns their home and console browsers.
func (w *World) Created(email, network string) (home, console *Browser) {
	w.T.Helper()
	home = w.OnHome(email)
	home.Get("/networks/new")
	if r := home.Post("/networks", CreateForm(network)); r.Status != http.StatusSeeOther {
		w.T.Fatalf("creating %s: %d %s", network, r.Status, r.Text())
	}
	if r := home.WaitReady(network); !strings.Contains(r.Text(), "ready") {
		w.T.Fatalf("%s never got ready:\n%s", network, r.Text())
	}
	home.Get("/networks/" + network)
	to, _ := url.Parse(home.Post("/networks/"+network+"/open", nil).Header.Get("Location"))
	console = w.Browser(network)
	if r := console.Handoff(to.RequestURI()); r.Status != http.StatusSeeOther {
		w.T.Fatalf("opening %s's console: %d", network, r.Status)
	}
	return home, console
}

// Reopen opens network's console again from home, as its owner coming back
// after a day away does (a sign-in unused a day ends): a new console
// browser, signed in.
func (w *World) Reopen(home *Browser, network string) *Browser {
	w.T.Helper()
	home.Get("/networks/" + network)
	to, _ := url.Parse(home.Post("/networks/"+network+"/open", nil).Header.Get("Location"))
	console := w.Browser(network)
	if r := console.Handoff(to.RequestURI()); r.Status != http.StatusSeeOther {
		w.T.Fatalf("opening %s's console again: %d", network, r.Status)
	}
	return console
}

// AddMember has actor add account to network's org, through its runner.
func (h *FakeHost) AddMember(t *testing.T, network, actor, account string) {
	t.Helper()
	h.mu.Lock()
	addr, s := h.Running[network], h.Specs[network]
	h.mu.Unlock()
	body, _ := json.Marshal(map[string]string{"account": account})
	req, _ := http.NewRequest("POST", addr.RunnerURL+"/v1/members", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.RunnerToken)
	req.Header.Set("GuildNet-Actor", actor)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("adding %s to %s: %s", account, network, resp.Status)
	}
}

// WaitRemoved waits until network has been removed and forgotten.
func (h *FakeHost) WaitRemoved(t *testing.T, network string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		h.mu.Lock()
		removed := slices.Contains(h.Removed, network)
		h.mu.Unlock()
		if _, err := h.w.Store.Network(context.Background(), network); removed && err != nil {
			return
		}
	}
	t.Fatalf("%s was never removed", network)
}

// SignedInHome is someone signed in on the home page with Google, on no
// list of any kind.
func SignedInHome(w *World, email string) *Browser {
	w.T.Helper()
	b := w.Home()
	if r := b.SignInWithGoogle(email); r.Status != http.StatusSeeOther {
		w.T.Fatalf("signing in as %s: %d %s", email, r.Status, r.Body)
	}
	return b
}

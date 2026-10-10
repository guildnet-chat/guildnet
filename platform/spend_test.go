// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"net/http"
	"strings"
	"testing"

	. "github.com/guildnet-chat/guildnet/platformtest"
)

// The owner's spend view in the console: today against the network's daily
// cap, this month, and each agent's share. Only for the owner.

func TestTheOwnerSeesWhatTheNetworkSpends(t *testing.T) {
	w := NewWorld(t)
	w.Networks["acme"].Ledger.RecordCost("kai", 1.25)
	ada := w.Browser("acme").SignedIn("ada")

	if !strings.Contains(ada.Get("/").Body, `href="/spend"`) {
		t.Error("the owner's sidebar has no Spend")
	}
	r := ada.Get("/spend")

	if r.Status != http.StatusOK {
		t.Fatalf("%d %s", r.Status, r.Text())
	}
	text := r.Text()
	for _, want := range []string{"$1.25", "$5.00", "25%"} {
		if !strings.Contains(text, want) {
			t.Errorf("the spend page doesn't show %q:\n%s", want, text)
		}
	}
	kai := RowFor(t, r.Body, ">kai<")
	for _, want := range []string{"claude-sonnet-5", "$1.00", "$1.25"} {
		if !strings.Contains(kai, want) {
			t.Errorf("kai's row doesn't show %q:\n%s", want, kai)
		}
	}
	if vera := RowFor(t, r.Body, ">vera<"); !strings.Contains(vera, "$0.00") {
		t.Errorf("vera, who spent nothing, should show $0.00:\n%s", vera)
	}
}

// Spend by scope: a lead sees their teams' agents (bo leads Sales, which
// has none yet); someone who may see no spend has no Spend at all.
func TestSpendShowsEachPersonTheAgentsTheyMaySee(t *testing.T) {
	w := NewWorld(t)
	w.Networks["acme"].Ledger.RecordCost("kai", 1.25)
	bo := w.Browser("acme").SignedIn("bo")
	cy := w.Browser("acme").SignedIn("cy")

	if !strings.Contains(bo.Get("/").Body, `href="/spend"`) {
		t.Error("bo leads Sales, so his sidebar should have Spend")
	}
	if r := bo.Get("/spend"); r.Status != http.StatusOK || strings.Contains(r.Text(), "$1.25") || strings.Contains(r.Body, ">kai<") {
		t.Errorf("bo at /spend: %d, want his teams' agents only, not kai's $1.25", r.Status)
	}
	if strings.Contains(cy.Get("/").Body, `href="/spend"`) {
		t.Error("cy's sidebar has Spend")
	}
	if r := cy.Get("/spend"); r.Status != http.StatusForbidden || strings.Contains(r.Text(), "$") {
		t.Errorf("cy at /spend: %d, want 403 and no numbers", r.Status)
	}
}

// SPDX-License-Identifier: AGPL-3.0-only

package ledger_test

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/ledger"
	"github.com/guildnet-chat/guildnet/models"
)

// AppendDetail is the per-call log the ledger bot reads to break spend down
// by model and by conversation, not just by agent's daily total.
func TestAppendDetailWritesOneJSONLineWithTokensModelAndCost(t *testing.T) {
	dir := t.TempDir()
	l, err := ledger.Open(dir, 5)
	if err != nil {
		t.Fatal(err)
	}

	cost, _ := l.Record("kai", "claude-sonnet-5", ledger.Usage{Input: 1000, Output: 500})
	l.AppendDetail(ledger.Detail{
		Agent: "kai",
		Model: "claude-sonnet-5",
		Where: "#engineering",
		Usage: ledger.Usage{Input: 1000, Output: 500},
		Cost:  cost,
	})

	path := filepath.Join(dir, "calls-"+time.Now().UTC().Format("2006-01-02")+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("detail log: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var lines int
	var d ledger.Detail
	for scanner.Scan() {
		lines++
		if err := json.Unmarshal(scanner.Bytes(), &d); err != nil {
			t.Fatalf("line %d: %v", lines, err)
		}
	}
	if lines != 1 {
		t.Fatalf("got %d lines, want 1", lines)
	}
	if d.Agent != "kai" || d.Model != "claude-sonnet-5" || d.Where != "#engineering" {
		t.Errorf("detail = %+v, missing agent/model/where", d)
	}
	if d.Usage.Input != 1000 || d.Usage.Output != 500 {
		t.Errorf("usage = %+v, want the recorded tokens", d.Usage)
	}
	if d.Cost != cost {
		t.Errorf("cost = %v, want %v", d.Cost, cost)
	}
	if d.Time.IsZero() {
		t.Error("time should default to now when not set")
	}
}

// Calls are charged at the catalogue's prices for the model, including its
// own cache rates (Opus 5.5 reads its cache at 5% of input, not 10%).
func TestCallsAreChargedAtTheCataloguesPrices(t *testing.T) {
	for _, c := range []struct {
		model string
		u     ledger.Usage
		want  float64
	}{
		{"claude-sonnet-5-5", ledger.Usage{Input: 1_000_000, Output: 100_000}, 2 + 1},
		{"claude-opus-5-5", ledger.Usage{CacheRead: 1_000_000, CacheWrite: 1_000_000}, 0.2 + 5},
		{"openai:gpt-6-luna", ledger.Usage{Input: 1_000_000, Output: 1_000_000}, 0.1 + 0.5},
		{"google:gemini-3.8-flash", ledger.Usage{Input: 1_000_000}, 0.75},
		// Not in the catalogue: charged at its dearest, so budgets stop early.
		{"claude-mystery-9", ledger.Usage{Input: 1_000_000, Output: 1_000_000}, 10 + 50},
	} {
		if got := ledger.Cost(c.model, c.u); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s %+v = $%v, want $%v", c.model, c.u, got, c.want)
		}
	}
	if got := ledger.CostAt("google:gemini-3.8-flash", ledger.Usage{Input: 1_000_000}, time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)); got != 1.5 {
		t.Errorf("Gemini 3.8 Flash in 2027 = $%v, want its new price $1.50", got)
	}
}

// Each call's detail says which price list charged it, so old spend can be
// explained after prices change.
func TestEachCallSaysWhichPricesChargedIt(t *testing.T) {
	dir := t.TempDir()
	l, _ := ledger.Open(dir, 5)
	l.AppendDetail(ledger.Detail{Agent: "kai", Model: "claude-sonnet-5-5"})
	b, _ := os.ReadFile(filepath.Join(dir, "calls-"+time.Now().UTC().Format("2006-01-02")+".jsonl"))
	var d ledger.Detail
	json.Unmarshal(b, &d)
	if d.Prices != models.Version {
		t.Errorf("prices %q, want the catalogue's %q", d.Prices, models.Version)
	}
}

// A closed ledger writes nothing more (its runner is stopping and its
// directory may be going), but still counts spend in memory.
func TestAClosedLedgerWritesNothingMore(t *testing.T) {
	dir := t.TempDir()
	l, err := ledger.Open(dir, 5)
	if err != nil {
		t.Fatal(err)
	}

	l.Close()
	l.RecordCost("kai", 1.25)
	l.AppendDetail(ledger.Detail{Agent: "kai", Model: "claude-sonnet-5"})

	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d files after closing", len(entries))
	}
	if spent, _ := l.Today(); spent != 1.25 {
		t.Errorf("today %v, want the spend still counted", spent)
	}
}

func TestAppendDetailErrorsAreNonFatalWhenDirIsGone(t *testing.T) {
	dir := t.TempDir()
	l, err := ledger.Open(dir, 5)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	// Must not panic even though the directory is gone.
	l.AppendDetail(ledger.Detail{Agent: "kai", Model: "claude-sonnet-5"})
}

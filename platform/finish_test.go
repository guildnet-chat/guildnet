// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/platformtest"
)

// A copy of the console told to stop (the old one, at a deploy) finishes
// setting up a network it began, rather than leaving it stuck.
func TestStoppingFinishesSettingUpANetworkFirst(t *testing.T) {
	w := platformtest.NewWorld(t)
	w.Host.HoldStarts()
	b := w.OnHome("bob@example.com")
	b.Get("/networks/new")
	if r := b.Post("/networks", platformtest.CreateForm("bobco")); r.Status != http.StatusSeeOther {
		t.Fatalf("creating bobco: %d %s", r.Status, r.Text())
	}

	finished := make(chan error, 1)
	go func() { finished <- w.Finish(10 * time.Second) }()
	select {
	case err := <-finished:
		t.Fatalf("it stopped while bobco was still being set up (%v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	w.Host.ReleaseStarts()
	if err := <-finished; err != nil {
		t.Errorf("finishing: %v", err)
	}
	if r := b.WaitReady("bobco"); !strings.Contains(r.Text(), "ready") {
		t.Errorf("bobco after the stop: %s", r.Text())
	}
}

// Nothing begun, nothing to wait for.
func TestStoppingWithNothingBegunIsAtOnce(t *testing.T) {
	w := platformtest.NewWorld(t)
	start := time.Now()
	if err := w.Finish(10 * time.Second); err != nil || time.Since(start) > time.Second {
		t.Errorf("finishing with nothing begun: %v after %v", err, time.Since(start))
	}
}

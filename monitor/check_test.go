// SPDX-License-Identifier: AGPL-3.0-only

package monitor_test

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/guildnet-chat/guildnet/monitor"
)

// A check made every few minutes (a runner asking the platform for its
// model keys, say) that fails once mends itself by the next: the platform
// was restarting. Each failure is logged; only one that keeps failing is
// reported.

func TestACheckIsReportedOnlyOnceItKeepsFailing(t *testing.T) {
	s := newSentry(t)
	start(t, monitor.Options{DSN: s.dsn(), Service: "runner"})
	var logged bytes.Buffer
	keys := &monitor.Check{Logger: log.New(&logged, "", 0)}
	down := errors.New("503 Service Unavailable")

	keys.Failed("model keys: %v", down)
	keys.OK()
	keys.Failed("model keys: %v", down)
	keys.Failed("model keys: %v", down)
	keys.OK()
	monitor.Flush()
	if sent := s.sent(); len(sent) != 0 {
		t.Fatalf("failures that mended themselves were reported: %v", sent)
	}
	if n := strings.Count(logged.String(), "model keys: 503 Service Unavailable\n"); n != 3 {
		t.Errorf("logged %d failures, want 3:\n%s", n, logged.String())
	}

	for range 5 {
		keys.Failed("model keys: %v", down)
	}
	monitor.Flush()
	sent := s.sent()
	if len(sent) != 1 || sent[0].Message != "model keys: 503 Service Unavailable" {
		t.Fatalf("sent %v, want the failure once", sent)
	}
	if len(sent[0].Fingerprint) != 1 || sent[0].Fingerprint[0] != "model keys: %v" {
		t.Errorf("fingerprint %q", sent[0].Fingerprint)
	}
}

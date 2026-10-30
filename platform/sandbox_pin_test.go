// SPDX-License-Identifier: AGPL-3.0-only

package platform_test

import (
	"crypto/tls"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/guildnet-chat/guildnet/pki"
	"github.com/guildnet-chat/guildnet/platform"
	. "github.com/guildnet-chat/guildnet/platformtest"
)

// sandboxd can be pinned by its key (GUILDNET_SANDBOX_PIN): the console then
// reaches only the sandboxd with that key, however good another's
// certificate from the same CA, and tells networks' runners to hold it to
// that too. Giving sandboxd a new key, and the console its pin, is how an
// old key (a desks host that was taken) stops being trusted.

// pinOf is the pin of the key sandboxd at url serves.
func pinOf(t *testing.T, url string) string {
	t.Helper()
	conn, err := tls.Dial("tcp", strings.TrimPrefix(url, "https://"), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return pki.KeyPin(conn.ConnectionState().PeerCertificates[0])
}

func TestTheConsoleReachesOnlyThePinnedSandboxd(t *testing.T) {
	real, other := remoteSandboxd(t, 200*24*time.Hour), remoteSandboxd(t, 200*24*time.Hour)
	for _, c := range []struct {
		pin     string
		reached bool
	}{{pinOf(t, real.URL), true}, {pinOf(t, other.URL), false}} {
		sb := real
		sb.Pin = c.pin
		w := NewSelfHostedWorld(t, func(o *Setup) { o.Sandbox = sb })
		_, ada := w.SetUp()

		page := ada.Get("/server").Text()

		if reached := strings.Contains(page, "certificate until"); reached != c.reached {
			t.Errorf("pinned to the right key %v: the console reached sandboxd: %v, want %v:\n%s", c.reached, reached, c.reached, page)
		}
	}
}

func TestTheBrokerTellsRunnersSandboxdsPin(t *testing.T) {
	const pin = "sha256/47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="
	w := NewWorldWith(t, func() Store { return platform.NewMemoryStore() }, AnyoneCreates, func(o *Setup) {
		o.Sandbox = platform.Sandbox{URL: "https://10.20.0.5:8443", Key: SandboxKey, Pin: pin}
	})
	w.Created("ada@example.com", "initech")

	status, out := BrokerCall(t, w, "initech", RunnerSecret(w, "initech"), "GET", "/sandbox")

	if status != http.StatusOK || out["pin"] != pin {
		t.Errorf("%d %v; want sandboxd's pin", status, out)
	}
	unpinned, _ := sandboxWorld(t)
	unpinned.Created("ada@example.com", "initech")
	if _, out := BrokerCall(t, unpinned, "initech", RunnerSecret(unpinned, "initech"), "GET", "/sandbox"); out["pin"] != nil {
		t.Errorf("with no pin set, the broker says %v", out["pin"])
	}
}

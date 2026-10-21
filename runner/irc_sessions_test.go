// SPDX-License-Identifier: AGPL-3.0-only

package runner_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// Someone can end their own IRC sessions (an app password revoked, a laptop
// lost), and the owner anyone's: the account is suspended and let in again
// at once, which ends every session of it. Nobody else can.
func TestEndingSomeonesIRCSessions(t *testing.T) {
	n := startNetwork(t, &fakeBrain{}, withAPI())
	inOrg(t, n.human, "dana")
	inOrg(t, n.human, "sam")

	if status, out := n.api("sam").call("POST", "/v1/people/dana/disconnect", nil); status != http.StatusForbidden {
		t.Errorf("sam ending dana's sessions: %d %v, want 403", status, out)
	}
	if status, out := n.api("dana").call("POST", "/v1/people/dana/disconnect", nil); status != http.StatusOK {
		t.Errorf("dana ending her own: %d %v", status, out)
	}
	if status, out := n.api("method").call("POST", "/v1/people/sam/disconnect", nil); status != http.StatusOK {
		t.Errorf("the owner ending sam's: %d %v", status, out)
	}

	// Being added let them in first: what counts comes after the first ADD.
	want := []string{"ADD dana", "DEL dana", "ADD sam", "DEL sam"}
	var got []string
	for range 100 {
		if got = suspensions(n.srv.NickServ()); len(got) >= len(want) {
			break
		}
		time.Sleep(waitTimeout / 100)
	}
	if !slices.Equal(got, want) {
		t.Errorf("NickServ was told %v", n.srv.NickServ())
	}
}

// suspensions are the SUSPENDs NickServ was told, from the first ADD on.
func suspensions(said []string) []string {
	var out []string
	for _, s := range said {
		if f := strings.Fields(s); len(f) >= 3 && f[0] == "SUSPEND" && (f[1] == "ADD" || len(out) > 0) {
			out = append(out, f[1]+" "+f[2])
		}
	}
	return out
}

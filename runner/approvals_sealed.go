// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/guildnet-chat/guildnet/internal/org"
	"github.com/guildnet-chat/guildnet/monitor"
)

// What an agent carries on from after an approval is its paused
// conversation, tool outputs and all: repo content, connector data. It's
// kept sealed (AES-GCM) with a key from the network's own secret, which only
// its runner has, bound to the agent, tool and summary of the approval it
// was paused for (what the approver was asked about), so it opens for no
// other. Without the secret it's kept plain, as one kept before was.
const resumeSealed = "sealed:"

func resumeAAD(a org.Approval) []byte {
	return []byte("approvals|resume|" + strings.ToLower(a.Agent) + "|" + a.Tool + "|" + a.Summary)
}

func (r *Runner) resumeGCM() (cipher.AEAD, error) {
	if r.cfg.Accounts.Secret == "" {
		return nil, errors.New("there's no network secret (GUILDNET_ACCOUNT_SECRET) to seal it with")
	}
	sum := sha256.Sum256([]byte("guildnet paused conversations\x00" + r.cfg.Accounts.Secret))
	return gcmWith(sum[:])
}

// sealResume is what a's agent carries on from, sealed; plain if there's no
// secret to seal it with.
func (r *Runner) sealResume(a org.Approval) string {
	gcm, err := r.resumeGCM()
	if err != nil || a.Resume == "" {
		return a.Resume
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return a.Resume
	}
	return resumeSealed + base64.RawStdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(a.Resume), resumeAAD(a)))
}

// openResume is what a's agent carries on from; plain says it was kept
// unsealed.
func (r *Runner) openResume(a org.Approval) (token string, plain bool, err error) {
	rest, sealed := strings.CutPrefix(a.Resume, resumeSealed)
	if !sealed {
		return a.Resume, a.Resume != "", nil
	}
	gcm, err := r.resumeGCM()
	if err != nil {
		return "", false, err
	}
	raw, err := base64.RawStdEncoding.DecodeString(rest)
	n := gcm.NonceSize()
	if err == nil && len(raw) >= n {
		if out, err := gcm.Open(nil, raw[:n], raw[n:], resumeAAD(a)); err == nil {
			return string(out), false, nil
		}
	}
	return "", false, errors.New("what it was doing couldn't be opened: sealed with another key, or for another approval")
}

// sealKept seals the conversations kept plain, from before they were sealed.
func (r *Runner) sealKept() {
	if _, err := r.resumeGCM(); err != nil {
		return
	}
	ctx := context.Background()
	pending, _ := r.store.PendingApprovals(ctx)
	decided, _ := r.store.Unresumed(ctx)
	for _, listed := range append(pending, decided...) {
		a, err := r.store.Approval(ctx, listed.ID)
		if err != nil {
			continue
		}
		if _, plain, _ := r.openResume(a); !plain {
			continue
		}
		if err := r.store.SetApprovalResume(ctx, a.ID, r.sealResume(a)); err != nil {
			monitor.Problem(r.logger, "sealing what #%d's agent carries on from: %v", a.ID, err)
		}
	}
}

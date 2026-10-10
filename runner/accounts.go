// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// accountsOn reports whether agents and bots get their own NickServ accounts.
func (r *Runner) accountsOn() bool {
	a := r.cfg.Accounts
	return a.APIURL != "" && a.APIToken != "" && a.Secret != ""
}

// accountPassword derives an agent's or bot's account password from the
// network secret, so there's no password list to store or leak.
func (r *Runner) accountPassword(nick string) string {
	mac := hmac.New(sha256.New, []byte(r.cfg.Accounts.Secret))
	mac.Write([]byte(strings.ToLower(nick)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ensureAccount registers nick's account if it doesn't exist yet.
func (r *Runner) ensureAccount(nick string) error {
	if !r.accountsOn() {
		return nil
	}
	ok, code, err := r.accountsAPI("/v1/ns/saregister", nick)
	if err != nil {
		return fmt.Errorf("registering %s: %w", nick, err)
	}
	if !ok && code != "ACCOUNT_EXISTS" {
		return fmt.Errorf("registering %s: %s", nick, code)
	}
	return nil
}

// claimAccount makes nick's account an agent's, for a new hire: one that
// already exists must be an agent's from before (its password is the one
// GuildNet derives), never a person's, who'd then speak as the agent.
func (r *Runner) claimAccount(nick string) error {
	if !r.accountsOn() {
		return nil
	}
	ok, code, err := r.accountsAPI("/v1/ns/saregister", nick)
	if err != nil {
		return refuse(failed, "couldn't make %s's IRC account just now: %v", nick, err)
	}
	if ok {
		return nil
	}
	if code != "ACCOUNT_EXISTS" {
		return refuse(invalid, "couldn't make %s's IRC account: %s.", nick, code)
	}
	if ours, _, err := r.accountsAPI("/v1/check_auth", nick); err != nil {
		return refuse(failed, "couldn't check %s's IRC account just now: %v", nick, err)
	} else if !ours {
		return refuse(conflict, "%s is already someone's IRC account: pick another name for the agent.", nick)
	}
	return nil
}

// accountsAPI calls Ergo's accounts API about nick, with its derived
// password: whether it succeeded, and its error code if not.
func (r *Runner) accountsAPI(path, nick string) (bool, string, error) {
	body, _ := json.Marshal(map[string]string{"accountName": nick, "passphrase": r.accountPassword(nick)})
	req, err := http.NewRequest("POST", strings.TrimRight(r.cfg.Accounts.APIURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return false, "", err
	}
	req.Header.Set("Authorization", "Bearer "+r.cfg.Accounts.APIToken)
	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	var out struct {
		Success   bool   `json:"success"`
		ErrorCode string `json:"errorCode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, "", fmt.Errorf("%s: %w", resp.Status, err)
	}
	return out.Success, out.ErrorCode, nil
}

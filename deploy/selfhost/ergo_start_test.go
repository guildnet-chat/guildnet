// SPDX-License-Identifier: AGPL-3.0-only

package selfhost_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Ergo's start.sh won't start a server a deployment has left without what
// it needs (the API's token, GuildServ's operator password, the web chat's
// key, the console's way in): it says what's missing instead. Run by the
// machine's own shell, with only the settings given.

// ergoNeeds is what every deployment gives Ergo (internal/host, compose.yml,
// GKE, Fargate, the dev stack).
var ergoNeeds = map[string]string{
	"ERGO__API__BEARER_TOKENS":          `["ergo-token"]`,
	"GUILDNET_OPER_PASSWORD":            "oper-password",
	"ERGO__ACCOUNTS__JWT_AUTH__ENABLED": "true",
	"ERGO__ACCOUNTS__JWT_AUTH__TOKENS":  `[{"algorithm": "hmac", "key": "chat-key", "account-claims": ["account"]}]`,
	"GUILDNET_IRC_AUTH_URL":             "https://broker:8082/irc/login",
	"GUILDNET_ERGO_TOKEN":               "ergo-token",
	"GUILDNET_BROKER_CA_FILE":           "/run/secrets/ca.crt",
}

// startErgo runs start.sh with these settings and says what it refused for
// ("" if it went on to start Ergo, which isn't here).
func startErgo(t *testing.T, env map[string]string) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh here")
	}
	cmd := exec.Command(sh, "../ergo/start.sh")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, _ := cmd.CombinedOutput()
	if s := string(out); strings.Contains(s, "refusing to start") {
		return s
	}
	return ""
}

func with(change map[string]string) map[string]string {
	env := map[string]string{}
	for k, v := range ergoNeeds {
		env[k] = v
	}
	for k, v := range change {
		if v == "" {
			delete(env, k)
		} else {
			env[k] = v
		}
	}
	return env
}

func TestErgoStartsWithWhatItNeeds(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"everything":              ergoNeeds,
		"an operator hash":        with(map[string]string{"GUILDNET_OPER_PASSWORD": "", "ERGO__OPERS__GUILDSERV__PASSWORD": "$2a$04$hash"}),
		"no web chat":             with(map[string]string{"ERGO__ACCOUNTS__JWT_AUTH__ENABLED": "", "ERGO__ACCOUNTS__JWT_AUTH__TOKENS": ""}),
		"no console to ask":       with(map[string]string{"GUILDNET_IRC_AUTH_URL": "", "GUILDNET_ERGO_TOKEN": "", "GUILDNET_BROKER_CA_FILE": ""}),
		"the console's CA inline": with(map[string]string{"GUILDNET_BROKER_CA_FILE": "", "GUILDNET_BROKER_CA": "Y2E="}),
	} {
		if refused := startErgo(t, env); refused != "" {
			t.Errorf("%s: %s", name, refused)
		}
	}
}

func TestErgoRefusesToStartWithoutWhatItNeeds(t *testing.T) {
	for name, c := range map[string]struct {
		env  map[string]string
		says string
	}{
		"no API token":         {with(map[string]string{"ERGO__API__BEARER_TOKENS": ""}), "ERGO__API__BEARER_TOKENS"},
		"an empty API token":   {with(map[string]string{"ERGO__API__BEARER_TOKENS": `[""]`}), "ERGO__API__BEARER_TOKENS"},
		"no operator password": {with(map[string]string{"GUILDNET_OPER_PASSWORD": ""}), "GUILDNET_OPER_PASSWORD"},
		"web chat with no key": {with(map[string]string{"ERGO__ACCOUNTS__JWT_AUTH__TOKENS": ""}), "ERGO__ACCOUNTS__JWT_AUTH__TOKENS"},
		"web chat, empty key":  {with(map[string]string{"ERGO__ACCOUNTS__JWT_AUTH__TOKENS": `[{"algorithm": "hmac", "key": "", "account-claims": ["account"]}]`}), "ERGO__ACCOUNTS__JWT_AUTH__TOKENS"},
		"a console, no token":  {with(map[string]string{"GUILDNET_ERGO_TOKEN": ""}), "GUILDNET_ERGO_TOKEN"},
		"a token, no console":  {with(map[string]string{"GUILDNET_IRC_AUTH_URL": "", "GUILDNET_BROKER_CA_FILE": ""}), "GUILDNET_IRC_AUTH_URL"},
		"https with no CA":     {with(map[string]string{"GUILDNET_BROKER_CA_FILE": ""}), "GUILDNET_BROKER_CA"},
	} {
		if refused := startErgo(t, c.env); !strings.Contains(refused, c.says) {
			t.Errorf("%s: start.sh said %q, want a refusal naming %s", name, refused, c.says)
		}
	}
}

// SPDX-License-Identifier: AGPL-3.0-only

package sandbox

import (
	"net"
	"regexp"
	"strings"
)

// Hosts a network's owner lets its desks reach, past DefaultAllowed: by
// name (each on 443, through its egress pass), never an address, never a
// name inside a network, and never one a blocklist names, whatever the
// owner says.

// DefaultBlocked are names no network's desks may reach: well-known places
// to drop what was taken (paste sites, file drops, request catchers) and
// tunnels into anywhere. Each blocks the name and every name under it.
var DefaultBlocked = []string{
	// Paste sites.
	"pastebin.com", "paste.ee", "hastebin.com", "rentry.co", "dpaste.com", "dpaste.org", "termbin.com", "ix.io", "paste.rs", "0x0.st", "ghostbin.site",
	// File drops.
	"transfer.sh", "file.io", "gofile.io", "catbox.moe", "temp.sh", "bashupload.com", "oshi.at", "filebin.net", "uguu.se", "pixeldrain.com", "wetransfer.com", "send.now",
	// Request catchers.
	"webhook.site", "requestbin.com", "requestcatcher.com", "pipedream.net", "beeceptor.com", "interact.sh", "oast.fun", "oast.pro", "oast.live", "oast.site",
	"oast.online", "oast.me", "oastify.com", "burpcollaborator.net", "dnslog.cn", "canarytokens.com",
	// Tunnels.
	"ngrok.io", "ngrok.app", "ngrok-free.app", "ngrok.dev", "trycloudflare.com", "loca.lt", "localtunnel.me", "serveo.net", "localhost.run", "lhr.life",
	"pinggy.io", "pinggy.link", "bore.pub", "playit.gg", "telebit.io",
	// Git hosting, and registries' publishing: what's sent there can be the
	// company's code, under someone else's name (the runner fetches desks'
	// repos for them, and reading a registry needs no publishing host).
	"github.com", "gitlab.com", "bitbucket.org", "codeberg.org",
	"registry.npmjs.org", "upload.pypi.org",
}

// internalNames are where names inside a network end: never reached by
// name from a desk, whatever the owner says.
var internalNames = []string{"localhost", "local", "internal", "lan", "home.arpa", "intranet", "corp", "home", "localdomain", "test", "example", "invalid", "onion"}

var hostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// CheckDeskHost is raw as a host desks may reach (lowercase, no trailing
// dot), or what's wrong with it: blocked is the install's own blocklist,
// beside DefaultBlocked.
func CheckDeskHost(raw string, blocked []string) (host, problem string) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	switch {
	case host == "":
		return "", "Give a host name, like api.example.com."
	case strings.ContainsAny(host, ":/@[]*?#"):
		return "", "Give just the host's name, like api.example.com: no https://, port, path or *."
	case net.ParseIP(host) != nil:
		return "", "Give the host's name, not its address: desks reach hosts by name."
	case len(host) > 253:
		return "", "That name is too long to be a host's."
	}
	labels := strings.Split(host, ".")
	for _, l := range labels {
		if !hostLabel.MatchString(l) {
			return "", host + " isn't a host name: use letters, digits, dashes and dots."
		}
	}
	last := labels[len(labels)-1]
	switch {
	case len(labels) < 2:
		return "", host + " is a name inside a network: desks only reach public hosts, by their full name."
	case strings.Trim(last, "0123456789") == "":
		return "", "Give the host's name, not its address: desks reach hosts by name."
	}
	for _, end := range internalNames {
		if host == end || strings.HasSuffix(host, "."+end) {
			return "", host + " is a name inside a network: desks only reach public hosts."
		}
	}
	if Blocked(host, blocked) {
		return "", host + " is blocked: desks never reach it, as it's a place to leave data or a tunnel into anywhere."
	}
	return host, ""
}

// Blocked says whether DefaultBlocked or blocked names host, or a name it's
// under.
func Blocked(host string, blocked []string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, list := range [][]string{DefaultBlocked, blocked} {
		for _, b := range list {
			b = strings.TrimSuffix(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(b)), "*."), ".")
			if b != "" && (host == b || strings.HasSuffix(host, "."+b)) {
				return true
			}
		}
	}
	return false
}

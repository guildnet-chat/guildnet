// SPDX-License-Identifier: AGPL-3.0-only

package platform

import "net/http"

// ircSettings is how to connect an IRC app to a network: its name on the
// base domain, the TLS port at the edge, and SASL with the person's account.
type ircSettings struct {
	Host    string
	Port    int // 0: IRC apps can't connect here
	Account string
}

func (p *Platform) ircSettings(network, account string) ircSettings {
	return ircSettings{Host: network + "." + p.opts.BaseDomain, Port: p.opts.IRCPort, Account: account}
}

func (p *Platform) connectPage(w http.ResponseWriter, r *http.Request) {
	s, _ := sessionOf(r)
	p.render(w, r, http.StatusOK, "connect", p.ircSettings(networkOf(r).Name, s.Account))
}

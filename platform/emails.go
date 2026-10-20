// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/guildnet-chat/guildnet/mail"
	"github.com/guildnet-chat/guildnet/monitor"
)

// tellOwner emails l to network n's owner, if GuildNet sends email: at the
// address they created it with on the home page or, for a network nobody
// owns there (set up on its own), the one its owner confirmed on their
// account page. It's only ever news: a failure is logged, and whatever it's
// about has happened anyway.
func (p *Platform) tellOwner(ctx context.Context, n Network, l Letter) {
	if p.opts.Mail == nil {
		return
	}
	// Sent even if the request that caused it has finished.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	switch {
	case n.Owner != "" && p.opts.People != nil:
		owner, err := p.opts.People.Person(ctx, n.Owner)
		if err != nil || owner.Email == "" {
			monitor.Problem(p.opts.Logger, "emailing %s's owner %q: no address: %v", n.Name, l.Subject, err)
			return
		}
		l.To = owner.Email
	case n.Owner == "" && n.OwnerNick != "" && p.opts.Alerts != nil:
		s, err := p.opts.Alerts.AlertSettings(ctx, n.Name, n.OwnerNick)
		if err != nil {
			monitor.Problem(p.opts.Logger, "emailing %s's owner %q: %v", n.Name, l.Subject, err)
			return
		}
		l.To = s.Email // none until they've confirmed one
		l.Paras = slices.Clone(l.Paras)
		for i := range l.Paras {
			l.Paras[i] = strings.ReplaceAll(l.Paras[i], notYou, notYouHere(n))
		}
	}
	if l.To == "" {
		return
	}
	if err := p.opts.Mail.Send(ctx, p.message(l)); err != nil {
		monitor.Problem(p.opts.Logger, "emailing %s's owner %q: %v", n.Name, l.Subject, err)
	}
}

// notYouHere is notYou for a network with no home page: its owner puts
// things right in its console.
func notYouHere(n Network) string {
	return "If that wasn't you, sign in to " + n.Name + "'s console, change your password on your account page, and check its keys in Settings."
}

// stamp is when something happened, for an email: "Tue 30 Sep 03:10 UTC".
func stamp(t time.Time) string { return t.UTC().Format("Mon 2 Jan 15:04 UTC") }

// Security alerts go to the owner whenever something changes that could hand
// the network to someone else. They can't be turned off.
const (
	securityWhy = "Security alerts go to a network's owner whenever its keys, its connections (GitHub, Linear and the like), the owner's password or anyone's two-step sign-in change. They can't be turned off."
	notYou      = "If that wasn't you, sign in to " + ourName + ", set a new IRC password from your network's page, and check its keys in Settings."
	ownerWhy    = "You're getting this because you own %s on " + ourName + "."
	welcomeWhy  = "You're getting this because you created %s on " + ourName + "."
	deletedWhy  = "You're getting this because you owned %s on " + ourName + "."
)

// welcomeEmail tells the owner their new network is ready.
func welcomeEmail(n Network, origin string) Letter {
	return Letter{Subject: n.Name + " is ready on " + ourName, Key: "welcome/" + n.Name + "/" + n.Updated.UTC().Format(time.RFC3339),
		Heading: n.Name + " is ready",
		Paras: []string{
			"Your network is up at " + strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://") + ", with its starting team of AI agents. You own it, and sign in to its IRC as " + n.OwnerNick + ".",
			"Good first steps: look over your agents and their roles, invite your people, and connect GitHub so the agents can work on your code.",
		},
		Button: "Open " + n.Name, Link: origin,
		After: []string{"Your agents run on your own model provider's key, within the daily spending cap in Settings."},
		Why:   fmt.Sprintf(welcomeWhy, n.Name),
	}
}

// joinedEmail tells the owner someone joined with an invite.
func joinedEmail(n Network, origin, nick string, l Link, at time.Time) Letter {
	as := ""
	switch {
	case l.Guest:
		as = " as a guest"
	case l.Team != "":
		as = " as " + l.Role + " in " + l.Team
	case l.Role != "":
		as = " as " + l.Role
	}
	return Letter{Subject: nick + " joined " + n.Name, Key: "joined/" + n.Name + "/" + l.ID,
		Heading: nick + " joined " + n.Name,
		Paras:   []string{nick + " joined " + n.Name + as + ", with the invite " + l.CreatedBy + " made, at " + stamp(at) + "."},
		Button:  "See your people", Link: origin + "/people",
		Why: fmt.Sprintf(ownerWhy, n.Name),
	}
}

// keyEmail alerts the owner that a model provider's key was added,
// replaced or removed, by who.
func keyEmail(n Network, origin, provider, what, who string, at time.Time) Letter {
	return Letter{Subject: n.Name + "'s " + provider + " key was " + what,
		Heading: n.Name + "'s " + provider + " key was " + what,
		Paras:   []string{who + " " + what + " the " + provider + " key on " + n.Name + ", at " + stamp(at) + ". The agents use the network's keys from now on.", notYou},
		Button:  "Open Settings", Link: origin + "/settings",
		Why: securityWhy,
	}
}

// passwordEmail alerts the owner that their IRC password was set from the
// home page.
func passwordEmail(n Network, page, email string, at time.Time) Letter {
	return Letter{Subject: "Your password on " + n.Name + " was changed",
		Heading: "Your password on " + n.Name + " was changed",
		Paras:   []string{"The IRC password for " + n.OwnerNick + " on " + n.Name + " was set from the " + ourName + " home page, signed in as " + email + ", at " + stamp(at) + ".", notYou},
		Button:  "Open " + n.Name + "'s page", Link: page,
		Why: securityWhy,
	}
}

// twoStepOffEmail alerts the owner that account's two-step sign-in on n was
// turned off or reset (how says which, and by who).
func twoStepOffEmail(n Network, origin, account, how string, at time.Time) Letter {
	return Letter{Subject: "Two-step sign-in for " + account + " on " + n.Name + " was " + strings.Fields(how)[0],
		Heading: "Two-step sign-in for " + account + " was " + strings.Fields(how)[0],
		Paras: []string{"Two-step sign-in for " + account + " on " + n.Name + " was " + how + ", at " + stamp(at) + ". Until it's set up again, " + account + "'s password alone signs in to the console.",
			notYou},
		Button: "Open " + n.Name, Link: origin + "/people",
		Why: securityWhy,
	}
}

// twoStepOnEmail alerts the owner that account's two-step sign-in on n was
// turned on (first) or added to, with way ("an authenticator app", "a
// passkey"): whoever did holds their account from then on.
func twoStepOnEmail(n Network, origin, account, way string, first bool, at time.Time) Letter {
	subject, heading := "Two-step sign-in for "+account+" on "+n.Name+" was turned on", "Two-step sign-in for "+account+" was turned on"
	what := "Two-step sign-in for " + account + " on " + n.Name + " was turned on, with " + way + ", at " + stamp(at) + ". From now on, signing in as " + account + " asks for it."
	if !first {
		thing := strings.TrimPrefix(strings.TrimPrefix(way, "an "), "a ")
		subject, heading = account+" added "+way+" on "+n.Name, account+" added "+way
		what = account + " added " + way + " to their two-step sign-in on " + n.Name + ", at " + stamp(at) + ". It signs in as them too, from now on, beside the " + thing + "s and app they had."
	}
	return Letter{Subject: subject, Heading: heading,
		Paras:  []string{what, "If " + account + " didn't do that, whoever did can sign in as them: reset their two-step sign-in from their page, and make them a password reset link."},
		Button: "See your people", Link: origin + "/people/" + account,
		Why: securityWhy,
	}
}

// resetMadeEmail alerts the owner that maker made a password reset link
// for account: whoever has it can sign in as them.
func resetMadeEmail(n Network, origin, maker, account string, at time.Time) Letter {
	return Letter{Subject: maker + " made a password reset link for " + account + " on " + n.Name,
		Heading: maker + " made a password reset link for " + account,
		Paras: []string{maker + " made a password reset link for " + account + " on " + n.Name + ", at " + stamp(at) + ". Whoever has it can set " + account + "'s password, once, within a day, and sign in as them.",
			"If that wasn't expected, ask " + maker + " about it, and have " + account + " set a new password in their account."},
		Button: "See your people", Link: origin + "/people",
		Why: securityWhy,
	}
}

// resetUsedEmail alerts the owner that account's password was reset with
// the link maker made.
func resetUsedEmail(n Network, origin, account, maker string, at time.Time) Letter {
	return Letter{Subject: account + "'s password on " + n.Name + " was reset",
		Heading: account + "'s password was reset",
		Paras: []string{account + "'s password on " + n.Name + " was set with the reset link " + maker + " made, at " + stamp(at) + ". Everywhere else " + account + " was signed in to the console, they're signed out.",
			"If " + account + " didn't do that, ask them, and make them a new reset link."},
		Button: "See your people", Link: origin + "/people",
		Why: securityWhy,
	}
}

// recoveryEmail alerts the owner that account signed in to n with one of
// their recovery codes.
func recoveryEmail(n Network, origin, account string, left int, at time.Time) Letter {
	return Letter{Subject: account + " used a recovery code on " + n.Name,
		Heading: account + " used a recovery code",
		Paras: []string{fmt.Sprintf("%s signed in to %s's console with one of their two-step recovery codes, at %s. They have %d left.", account, n.Name, stamp(at), left),
			"People use one when they don't have their authenticator app to hand. " + notYou},
		Button: "Open " + n.Name, Link: origin,
		Why: securityWhy,
	}
}

// githubEmail alerts the owner that the network was connected to GitHub,
// or disconnected.
func githubEmail(n Network, page, what, account, where string, at time.Time) Letter {
	to := "to"
	if what == "disconnected" {
		to = "from"
	}
	return Letter{Subject: n.Name + " was " + what + " " + to + " GitHub",
		Heading: n.Name + " was " + what + " " + to + " GitHub",
		Paras: []string{n.Name + " was " + what + " " + to + " the GitHub account " + account + " from " + where + ", at " + stamp(at) + ".",
			notYou},
		Button: "Open " + n.Name + "'s page", Link: page,
		Why: securityWhy,
	}
}

// confirmEmail asks whoever has address to confirm it for account's alerts
// on network.
func confirmEmail(address, account, network, link string) Letter {
	return Letter{To: address, Subject: "Confirm your email for " + network + " on " + ourName,
		Heading: "Confirm your email",
		Paras:   []string{account + " asked for " + ourName + " alerts from " + network + " to come to this address. Confirm it's yours, and they can."},
		Button:  "Confirm this address", Link: link,
		After: []string{"The link works once, within a day. Nothing is sent here until it's confirmed, and you choose which alerts on your account page."},
		Why:   "If you didn't ask for this, you can ignore it: nothing will be sent here.",
	}
}

// approvalEmail tells an approver an approval is waiting on them. An outside
// agent's request in its own words (ownWords) is marked as theirs.
func approvalEmail(n Network, origin string, id int64, agent, summary, where, asker, movedFrom string, expires time.Time, ownWords bool) Letter {
	summary = oneLine(summary, 300)
	short := oneLine(summary, 90)
	wants := agent + " wants to " + summary
	if ownWords {
		wants = agent + " asks, in its own words, to “" + summary + "”"
		short = "“" + short + "”"
	}
	paras := []string{wants + ", for " + asker + " in " + where + "."}
	if movedFrom != "" {
		paras = append(paras, "It's come to you because "+movedFrom+" didn't answer.")
	}
	if !expires.IsZero() {
		paras = append(paras, "It expires "+stamp(expires)+".")
	}
	ids := fmt.Sprint(id)
	return Letter{Subject: "#" + ids + ": " + agent + " needs your approval to " + short, Key: "approval/" + n.Name + "/" + ids + "/" + movedFrom,
		Heading: agent + " needs your approval",
		Paras:   paras,
		Button:  "Review it", Link: origin + "/approvals",
		After: []string{"Or answer GuildServ in chat: APPROVE " + ids + ", or DENY " + ids + " with a reason."},
		Why:   "You're getting this because you turned on approval emails for " + n.Name + ". Turn them off on your account page.",
	}
}

// capEmail tells the owner the network spent its daily cap.
func capEmail(n Network, origin string, spent, cap float64, at time.Time) Letter {
	return Letter{Subject: n.Name + " reached its daily spending cap", Key: "cap/" + n.Name + "/" + at.UTC().Format("2006-01-02"),
		Heading: n.Name + " reached its daily spending cap",
		Paras: []string{fmt.Sprintf("%s's agents have spent $%.2f today, reaching the network's daily cap of $%.2f, so they've stopped until the day starts again at 00:00 UTC.", n.Name, spent, cap),
			"People can still talk in the network; the agents pick up again tomorrow."},
		Button: "See spending", Link: origin + "/spend",
		Why: fmt.Sprintf(ownerWhy, n.Name),
	}
}

// oneLine is s on one line, at most max bytes.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = strings.TrimSpace(s[:max]) + "…"
	}
	return s
}

// deletedEmail tells the owner their network was deleted.
func deletedEmail(n Network, home, who string, at time.Time) Letter {
	return Letter{Subject: n.Name + " was deleted", Key: "deleted/" + n.Name + "/" + at.UTC().Format(time.RFC3339),
		Heading: n.Name + " was deleted",
		Paras: []string{who + " deleted " + n.Name + " at " + stamp(at) + ". Its agents, chat history and settings are being removed, and its address is free again.",
			"If that wasn't you, reply to this email straight away."},
		Button: "Your networks", Link: home,
		Why: fmt.Sprintf(deletedWhy, n.Name),
	}
}

// Letter is one of GuildNet's emails, in parts, so its plain text and its
// HTML (in the site's style: templates/email.html) say the same: a
// heading, what it says, one thing to do (a button, and the link written
// out), and why they got it.
type Letter struct {
	To, Subject string
	Key         string   // sent once for this key, even if retried
	Heading     string   // the HTML's title; the text starts "Hi,"
	Paras       []string // what it says, before the button
	Button      string   // the one thing to do
	Link        string   // where it goes
	After       []string // after the button
	Why         string   // why they got it, at the foot
}

type emailView struct {
	Letter
	Home, Logo, Preheader string
}

// message is l as an email: plain text, and the same in HTML.
func (p *Platform) message(l Letter) mail.Message {
	// Written in our name (ourName), sent in the brand's.
	l.Subject, l.Heading, l.Button, l.Why = p.inOurName(l.Subject), p.inOurName(l.Heading), p.inOurName(l.Button), p.inOurName(l.Why)
	l.Paras, l.After = slices.Clone(l.Paras), slices.Clone(l.After)
	for i := range l.Paras {
		l.Paras[i] = p.inOurName(l.Paras[i])
	}
	for i := range l.After {
		l.After[i] = p.inOurName(l.After[i])
	}
	text := "Hi,\n\n" + strings.Join(l.Paras, "\n\n") + "\n\n" + l.Button + ":\n" + l.Link + "\n\n"
	if len(l.After) > 0 {
		text += strings.Join(l.After, "\n\n") + "\n\n"
	}
	text += "-- \n" + l.Why + "\n"
	m := mail.Message{To: l.To, Subject: l.Subject, Text: text, Key: l.Key}
	home := p.scheme() + "://" + p.opts.BaseDomain
	v := emailView{Letter: l, Home: home + "/", Logo: home + "/apple-touch-icon.png"}
	if len(l.Paras) > 0 {
		v.Preheader = l.Paras[0]
	}
	var b strings.Builder
	if err := p.email.Execute(&b, v); err != nil {
		// Still worth sending: the text says it all.
		monitor.Problem(p.opts.Logger, "email %q: its HTML: %v", l.Subject, err)
		return m
	}
	m.HTML = b.String()
	return m
}

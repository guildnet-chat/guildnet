// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/ergochat/irc-go/ircmsg"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/roles"
	"github.com/guildnet-chat/guildnet/models"
)

// GuildServ is the control plane's IRC face, in the tradition of NickServ
// and ChanServ: /msg GuildServ HELP.
const guildServNick = "GuildServ"

// Defaults for a newly hired agent. Its model is the balanced tier's, from
// the network's providers.
const (
	defaultEffort = "medium"
	defaultBudget = 1.00
)

// modelFor is the model name means here (a model, or a tier: "fast",
// "balanced", "best"), or why it can't be used.
func (r *Runner) modelFor(name string) (models.Model, string) {
	m, err := models.Resolve(name, r.cfg.ModelProviders())
	switch {
	case errors.Is(err, models.ErrUnknown):
		return m, fmt.Sprintf("%s isn't a model GuildNet knows. Use a tier (fast, balanced, best) or a model like openai:gpt-6-sol.", strings.TrimSpace(name))
	case errors.Is(err, models.ErrNoKey):
		if known, ok := models.Find(name); ok {
			return m, fmt.Sprintf("%s is %s's, and this network has no %s key. The owner adds one in the console's settings.", known.Title, known.Provider.Title(), known.Provider.Title())
		}
		return m, "this network has no model provider's key yet. The owner adds one in the console's settings."
	case err != nil:
		return m, err.Error()
	}
	return m, ""
}

// smallModel is the model for a small call (a catch-up, picking who
// answers) named name, a tier or a model, from the providers the network has
// now. If it can't be used, name goes as it is, and the call says why.
func (r *Runner) smallModel(name string) string {
	m, err := models.Resolve(name, r.cfg.ModelProviders())
	if err != nil {
		return name
	}
	return m.ID
}

// modelAbout is the model named id for people: its title and what it costs
// now; a model GuildNet doesn't know is only its name.
func (r *Runner) modelAbout(id string) (title, pricing string) {
	m, ok := models.Find(id)
	if !ok {
		return id, ""
	}
	return m.Title, m.Pricing(r.now())
}

// defaultModel is a new hire's model: the balanced tier's.
func (r *Runner) defaultModel() string {
	m, _ := models.ForTier(models.Balanced, r.cfg.ModelProviders())
	return m.ID
}

var defaultChannels = []string{"#general"}

// fields are the settable parts of an agent's profile, as GuildServ names them.
var fields = []string{"PRESET", "NAME", "ROLE", "TEAM", "REPORTS-TO", "MODEL", "EFFORT", "CHANNELS", "BUDGET", "PROMPT", "PLATFORM", "WAKES"}

func (r *Runner) onGuildServ(e ircmsg.Message) {
	if len(e.Params) < 2 || isChannel(e.Params[0]) {
		return
	}
	from := e.Nick()
	if isService(from) || r.isAgent(from) || strings.EqualFold(from, r.cfg.LedgerNick) {
		return
	}
	// Who's asking: the account the server vouches for, never the nick, since
	// anyone can take a nick that isn't logged in.
	actor := ""
	if ok, account := e.GetTag("account"); ok {
		actor = account
	}
	r.guildCommand(from, actor, e.Params[1], false)
}

// guildCommand runs a GuildServ command from actor, answering from. Over
// plain IRC (console false), those who sign in with two steps may only look
// (twostep.go); from the console, which has seen both steps, they may change
// things too.
func (r *Runner) guildCommand(from, actor, text string, console bool) {
	words, err := splitArgs(text)
	if err != nil || len(words) == 0 {
		r.guildReply(from, "couldn't read that. Try HELP.")
		return
	}
	cmd, args := word(words[0]), words[1:]
	// GuildServ is the company's: someone with an account on the server who
	// isn't in the org (or isn't logged in) gets no look at it, and no
	// catch-ups on the network's budget.
	if cmd != "HELP" && !r.inOrgAsPerson(actor) {
		if actor == "" {
			r.guildReply(from, "GuildServ answers people in this company: log in to your account (SASL, or /msg NickServ IDENTIFY) first.")
		} else {
			r.guildReply(from, "GuildServ answers people in this company, and "+actor+" isn't in it.")
		}
		return
	}
	if !console && changesTheOrg(cmd, args) {
		if why := r.notOverIRC(actor); why != "" {
			r.guildReply(from, why)
			return
		}
	}

	switch cmd {
	case "HELP":
		r.guildReply(from, "Anyone in the company: CATCHUP [#channel] [3h] (one every 10 minutes), STAFF, ORG, TEAMS, BUDGET [team], INFO <name>, COMPANY, GOALS [team], ROLES [department] (the role catalogue), ACCESS (who may do what). Approvers: APPROVALS, APPROVE <n> [note], DENY <n> <reason>. Owner: APPROVALS AFTER <4h|off> (how long one waits on each person).")
		r.guildReply(from, "Changing the org (what you may do depends on your access; the owner may do anything): HIRE <nick> [PRESET role] [ROLE \"...\"] [TEAM ...] [REPORTS-TO name] [MODEL ...] [CHANNELS #a,#b] [BUDGET usd] [PROMPT \"...\"], SET <nick> <field> <value>, FIRE <nick> [\"reason\"], TEAM CREATE <name> [PARENT team] [LEAD name] [CHANNEL #x] [BUDGET usd/month] [PRIVATE].")
		r.guildReply(from, "TEAM SET <name> LEADS|PARENT|FOCUS|CHANNEL|BUDGET|PRIVATE <value>, COMPANY DESCRIBE \"...\", COMPANY GUESTS on|off (owner: whether guests may have agents work for them), GOAL ADD \"...\" [TEAM name] [MEASURE \"...\" TARGET x CURRENT y] [SERVES n], GOAL SET <n> <field> <value>, GOAL DROP <n>.")
		r.guildReply(from, "MEMBER ADD|SET|REMOVE <account> ... (COST usd/month), PROMOTE <name> [team], DEMOTE <name> [team], ACCESS GIVE|TAKE <person> <level>, LOG [n]. Owner: ACCESS SET <level> <abilities,...> [SCOPE org|led|reports|own|teams] [TEAMS a,b], ACCESS DROP <level>.")
	case "STAFF":
		r.guildStaff(from)
	case "ORG":
		r.guildOrg(from)
	case "TEAMS":
		r.guildTeams(from, actor)
	case "BUDGET":
		r.guildBudget(from, actor, args)
	case "CATCHUP":
		r.guildCatchup(from, actor, args)
	case "APPROVE", "DENY", "APPROVALS":
		if actor == "" {
			r.guildReply(from, "log in to your account (SASL, or /msg NickServ IDENTIFY) to decide approvals.")
			return
		}
		if cmd == "APPROVALS" {
			r.guildApprovals(from, actor, args)
		} else {
			r.guildDecide(from, actor, cmd == "APPROVE", args)
		}
	case "INFO":
		r.guildInfo(from, args)
	case "ROLES":
		r.guildRoles(from, args)
	case "GOALS":
		r.guildGoals(from, actor, args)
	case "COMPANY":
		r.guildCompany(from, actor, args)
	case "HIRE", "FIRE", "SET", "TEAM", "MEMBER", "PROMOTE", "DEMOTE", "ACCESS", "LOG", "GOAL":
		if actor == "" {
			r.guildReply(from, "log in to your account (SASL, or /msg NickServ IDENTIFY) first: what you may change depends on who you are.")
			return
		}
		switch cmd {
		case "HIRE":
			r.guildHire(from, actor, args)
		case "FIRE":
			r.guildFire(from, actor, args)
		case "SET":
			r.guildSet(from, actor, args)
		case "TEAM":
			r.guildTeam(from, actor, args)
		case "MEMBER":
			r.guildMember(from, actor, args)
		case "PROMOTE":
			r.guildPromote(from, actor, args, true)
		case "DEMOTE":
			r.guildPromote(from, actor, args, false)
		case "ACCESS":
			r.guildAccess(from, actor, args)
		case "LOG":
			r.guildLog(from, actor, args)
		case "GOAL":
			r.guildGoal(from, actor, args)
		}
	default:
		r.guildReply(from, fmt.Sprintf("unknown command %s. Try HELP.", cmd))
	}
}

// word is a command or field name as GuildServ knows it: in capitals, with
// the names from when teams were called lanes (LANE, LANES) still understood.
func word(s string) string {
	s = strings.ToUpper(s)
	switch s {
	case "LANE":
		return "TEAM"
	case "LANES":
		return "TEAMS"
	}
	return s
}

// pairs reads "FIELD value FIELD value ..." arguments.
func pairs(args []string) ([]field, string) {
	var out []field
	for len(args) > 0 {
		if len(args) < 2 {
			return nil, fmt.Sprintf("%s needs a value.", strings.ToUpper(args[0]))
		}
		out = append(out, field{name: args[0], value: args[1]})
		args = args[2:]
	}
	return out, ""
}

func (r *Runner) guildHire(from, actor string, args []string) {
	if len(args) == 0 {
		r.guildReply(from, "usage: HIRE <nick> [PRESET role] [ROLE \"...\"] [TEAM ...] [REPORTS-TO name] [MODEL ...] [CHANNELS #a,#b] [BUDGET usd]")
		return
	}
	fields, msg := pairs(args[1:])
	if msg != "" {
		r.guildReply(from, msg)
		return
	}
	p, notes, err := r.hireAgent(actor, args[0], fields)
	if err != nil {
		r.guildReply(from, err.Error())
		return
	}
	r.guildReply(from, fmt.Sprintf("hired %s (%s), reporting to %s. Joining %s.", p.Nick, p.Role, p.ReportsTo, strings.Join(p.Channels, ", ")))
	for _, n := range notes {
		r.guildReply(from, n)
	}
}

func (r *Runner) guildFire(from, actor string, args []string) {
	if len(args) == 0 {
		r.guildReply(from, `usage: FIRE <nick> ["reason"]`)
		return
	}
	nick, reason := args[0], strings.TrimSpace(strings.Join(args[1:], " "))
	if err := r.fireAgent(actor, nick, reason); err != nil {
		r.guildReply(from, err.Error())
		return
	}
	r.guildReply(from, fmt.Sprintf("fired %s. They've left the network.", nick))
}

func (r *Runner) guildSet(from, actor string, args []string) {
	if len(args) < 3 {
		r.guildReply(from, fmt.Sprintf("usage: SET <nick> <field> <value>. Fields: %s.", strings.Join(fields, ", ")))
		return
	}
	change := field{name: args[1], value: strings.Join(args[2:], " ")}
	p, notes, err := r.setAgent(actor, args[0], []field{change})
	if err != nil {
		r.guildReply(from, err.Error())
		return
	}
	r.guildReply(from, fmt.Sprintf("%s's %s is now %s.", p.Nick, strings.ToLower(change.name), describeField(p, change.name)))
	for _, n := range notes {
		r.guildReply(from, n)
	}
}
func (r *Runner) guildInfo(from string, args []string) {
	if len(args) != 1 {
		r.guildReply(from, "usage: INFO <nick>")
		return
	}
	ctx := context.Background()
	if p, err := r.store.Agent(ctx, args[0]); err == nil {
		// Its id too, which is what SET ... MODEL takes.
		model, pricing := r.modelAbout(p.Model)
		if pricing != "" {
			model += " (" + p.Model + "; " + pricing + ")"
		}
		works := fmt.Sprintf("Model %s, effort %s, budget $%.2f/day.", model, p.Effort, p.DailyBudgetUSD)
		if p.Platform == PlatformOutside {
			works = "Works outside GuildNet, " + r.outsideStatus(p.Nick) + "."
		}
		info := fmt.Sprintf("%s (%s, AI agent): %s, %s, reports to %s. %s Channels: %s.",
			p.Nick, p.Name, p.Role, r.teamLabel(p.Nick, p.Team), p.ReportsTo, works, strings.Join(p.Channels, ", "))
		for _, chunk := range wrap(info, maxLineBytes) {
			r.guildReply(from, chunk)
		}
		return
	}
	m, err := r.store.Member(ctx, args[0])
	if err != nil {
		r.guildStoreError(from, args[0], err)
		return
	}
	kind := "human"
	if m.Guest {
		kind = "guest"
	}
	if r.isOwner(m.Account) {
		r.guildReply(from, fmt.Sprintf("%s (%s): %s, owner of the network.", m.Account, kind, m.Role))
		return
	}
	r.guildReply(from, fmt.Sprintf("%s (%s): %s, %s, reports to %s.", m.Account, kind, m.Role, r.teamLabel(m.Account, m.Team), m.ReportsTo))
}

func (r *Runner) guildStaff(from string) {
	agents := r.staff()
	if len(agents) == 0 {
		r.guildReply(from, "no agents on staff yet.")
		return
	}
	parts := make([]string, 0, len(agents))
	for _, p := range agents {
		parts = append(parts, fmt.Sprintf("%s (%s)", p.Nick, p.Role))
	}
	for _, chunk := range wrap("staff: "+strings.Join(parts, ", "), maxLineBytes) {
		r.guildReply(from, chunk)
	}
}

// applyField sets one profile field from its GuildServ name and returns a
// problem to report, or "".
func (r *Runner) applyField(p *config.Persona, field, value string) string {
	switch word(field) {
	case "PRESET":
		role, ok := roles.Find(value)
		if !ok {
			return fmt.Sprintf("there's no role %q in the catalogue. ROLES lists them.", value)
		}
		m, msg := r.modelFor(string(role.Tier))
		if msg != "" {
			return msg
		}
		p.Role, p.Model, p.Effort, p.DailyBudgetUSD, p.Prompt = role.Title, m.ID, role.Effort, role.DailyBudget, role.Prompt
	case "NAME":
		p.Name = value
	case "ROLE":
		p.Role = value
	case "TEAM":
		l, msg := r.checkTeam(value)
		if msg != "" {
			return msg
		}
		p.Team = l
	case "REPORTS-TO":
		if !r.inOrg(value) {
			return fmt.Sprintf("%s isn't in the org.", value)
		}
		if r.reportsLoop(p.Nick, value) {
			return fmt.Sprintf("%s can't report to %s: %s already reports up to %s, so that would make a reporting loop.", p.Nick, value, value, p.Nick)
		}
		p.ReportsTo = value
	case "MODEL":
		m, msg := r.modelFor(value)
		if msg != "" {
			return msg
		}
		p.Model = m.ID
	case "EFFORT":
		switch value {
		case "low", "medium", "high", "xhigh", "max":
			p.Effort = value
		default:
			return "EFFORT is one of low, medium, high, xhigh, max."
		}
	case "CHANNELS":
		var chans []string
		for _, ch := range strings.Split(value, ",") {
			ch = strings.TrimSpace(ch)
			if !validChannel(ch) {
				return fmt.Sprintf("%q isn't a channel; use #names separated by commas.", ch)
			}
			chans = append(chans, ch)
		}
		p.Channels = chans
	case "BUDGET":
		usd, err := strconv.ParseFloat(strings.TrimPrefix(value, "$"), 64)
		if err != nil || usd < 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
			return "BUDGET is dollars per day, like 2.50."
		}
		p.DailyBudgetUSD = usd
	case "PROMPT":
		p.Prompt = value
	case "WAKES":
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 10000 {
			return "WAKES is how many times a day an outside agent may be woken, 1 to 10000."
		}
		p.DailyWakes = n
	case "PLATFORM":
		v := strings.ToLower(value)
		if v == "chat" {
			v = ""
		}
		if v != "" && v != PlatformDesk && v != PlatformManaged && v != PlatformOutside {
			return "PLATFORM is chat (GuildNet's tools, any model), desk (a workspace too, in GuildNet's sandbox, any model), managed (a desk on Anthropic's Managed Agents, Claude only), or outside (its own program, on your systems)."
		}
		if v == PlatformOutside {
			if why := r.outsideReady(); why != "" {
				return why
			}
			p.Platform = v
			return ""
		}
		if hp, ok := r.brain.(interface{ HasPlatform(string) bool }); !ok || !hp.HasPlatform(v) {
			switch v {
			case PlatformDesk:
				return "Desks aren't set up on this network yet (they need GuildNet's sandbox), so agents can't move there."
			case PlatformManaged:
				return "Managed Agents isn't set up on this network, so agents can't move there yet."
			}
		}
		p.Platform = v
	default:
		return fmt.Sprintf("unknown field %s. Fields: %s.", strings.ToUpper(field), strings.Join(fields, ", "))
	}
	return ""
}

func describeField(p config.Persona, field string) string {
	switch word(field) {
	case "CHANNELS":
		return strings.Join(p.Channels, ", ")
	case "BUDGET":
		return fmt.Sprintf("$%.2f/day", p.DailyBudgetUSD)
	case "PROMPT":
		return "updated"
	case "WAKES":
		return fmt.Sprintf("%d a day", wakeCap(p))
	case "PLATFORM":
		if p.Platform == "" {
			return "chat"
		}
		return p.Platform
	case "NAME":
		return p.Name
	case "ROLE":
		return p.Role
	case "TEAM":
		return p.Team
	case "REPORTS-TO":
		return p.ReportsTo
	case "MODEL":
		return p.Model
	case "EFFORT":
		return p.Effort
	}
	return ""
}

// checkNewNick returns why nick can't be hired, or "".
func (r *Runner) checkNewNick(nick string) string {
	for i, c := range nick {
		ok := c < unicode.MaxASCII && (unicode.IsLetter(c) || (i > 0 && (unicode.IsDigit(c) || c == '_' || c == '-')))
		if !ok {
			return fmt.Sprintf("%q isn't a usable nick: letters first, then letters, digits, _ or -.", nick)
		}
	}
	if len(nick) > 30 {
		return "nicks can be at most 30 characters."
	}
	if r.inOrg(nick) {
		return fmt.Sprintf("%s is already in the org.", nick)
	}
	for _, taken := range []string{r.cfg.Owner, r.cfg.LedgerNick, guildServNick} {
		if strings.EqualFold(nick, taken) {
			return fmt.Sprintf("%s is already taken.", nick)
		}
	}
	if isService(nick) {
		return "nicks ending in \"serv\" are reserved for services."
	}
	return ""
}

func (r *Runner) guildStoreError(to, nick string, err error) {
	r.guildReply(to, r.storeRefusal(nick, err).Error())
}
func (r *Runner) guildReply(to, text string) {
	r.guildConn.Notice(to, truncate(text, maxLineBytes))
}

// splitArgs splits a command line on spaces, keeping "quoted phrases" whole.
func splitArgs(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inQuote, have := false, false
	for _, c := range s {
		switch {
		case c == '"':
			inQuote = !inQuote
			have = true
		case c == ' ' && !inQuote:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteRune(c)
			have = true
		}
	}
	if inQuote {
		return nil, errors.New("unclosed quote")
	}
	if have {
		out = append(out, cur.String())
	}
	return out, nil
}

// guildRoles runs ROLES [department]: the role catalogue to hire from.
func (r *Runner) guildRoles(from string, args []string) {
	dept := strings.Join(args, " ")
	if dept == "" {
		r.guildReply(from, "departments: "+strings.Join(roles.Departments(), ", ")+". ROLES <department> lists its roles; HIRE <nick> PRESET <role> hires one.")
		return
	}
	list := roles.In(dept)
	if len(list) == 0 {
		r.guildReply(from, fmt.Sprintf("no department %q. Departments: %s.", dept, strings.Join(roles.Departments(), ", ")))
		return
	}
	for _, role := range list {
		r.guildReply(from, truncate(fmt.Sprintf("%s (%s): %s %s", role.Key, role.Title, role.About, role.Today), maxLineBytes))
	}
}

// presetFirst puts a PRESET field first, so whatever else is said wins over
// what the preset fills in.
func presetFirst(fs []field) []field {
	out := make([]field, 0, len(fs))
	for _, f := range fs {
		if word(f.name) == "PRESET" {
			out = append(out, f)
		}
	}
	for _, f := range fs {
		if word(f.name) != "PRESET" {
			out = append(out, f)
		}
	}
	return out
}

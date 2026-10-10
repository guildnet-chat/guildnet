// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/guildnet-chat/guildnet/internal/org"
)

// What the company and its teams are for: the company's description and
// goals, and each team's focus and objectives (goals of its own, which can
// serve one of the company's). They're the guiding lights every agent reads.
// The company's are changed by whoever may arrange the whole org (the owner
// and admins); a team's by whoever may arrange the team (its leads).

const (
	maxDescription = 2000
	maxGoalText    = 300
	maxMeasure     = 100
)

// setCompany sets the company's description as actor.
func (r *Runner) setCompany(actor, description string) error {
	description = strings.TrimSpace(description)
	if len(description) > maxDescription {
		return refuse(invalid, "keep the description to %d characters; it goes to every agent.", maxDescription)
	}
	if err := r.allowed(actor, canArrange, wholeOrg, "describe the company"); err != nil {
		return err
	}
	if err := r.store.SetCompany(context.Background(), description); err != nil {
		return r.storeRefusal("the company", err)
	}
	r.logChange(actor, "describe company", "", "", description)
	r.updateWelcome(false)
	return nil
}

// goalFields applies GuildServ-style fields (TEXT, TEAM, MEASURE, TARGET,
// CURRENT, SERVES) to g.
func (r *Runner) applyGoalField(g *org.Goal, field, value string) string {
	value = strings.TrimSpace(value)
	one := func(max int) string {
		if len(value) > max || strings.ContainsAny(value, "\r\n") {
			return fmt.Sprintf("%s is one line, at most %d characters.", strings.ToUpper(field), max)
		}
		return ""
	}
	switch word(field) {
	case "TEXT":
		if value == "" {
			return "a goal needs words."
		}
		if msg := one(maxGoalText); msg != "" {
			return msg
		}
		g.Text = value
	case "TEAM":
		if value == "" || strings.EqualFold(value, "none") {
			g.Team = ""
			break
		}
		team, msg := r.checkTeam(value)
		if msg != "" {
			return msg
		}
		g.Team = team
	case "MEASURE", "TARGET", "CURRENT":
		if msg := one(maxMeasure); msg != "" {
			return msg
		}
		switch word(field) {
		case "MEASURE":
			g.Measure = value
		case "TARGET":
			g.Target = value
		default:
			g.Current = value
		}
	case "SERVES":
		if value == "" || strings.EqualFold(value, "none") || value == "0" {
			g.Serves = 0
			break
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(value, "#"), 10, 64)
		if err != nil {
			return "SERVES is the number of one of the company's goals, like 1."
		}
		if served, ok := r.goal(id); !ok || served.Team != "" {
			return fmt.Sprintf("there's no company goal #%d to serve.", id)
		}
		g.Serves = id
	default:
		return fmt.Sprintf("unknown goal field %s: use TEXT, TEAM, MEASURE, TARGET, CURRENT, or SERVES.", strings.ToUpper(field))
	}
	return ""
}

func (r *Runner) goal(id int64) (org.Goal, bool) {
	goals, _ := r.store.Goals(context.Background())
	for _, g := range goals {
		if g.ID == id {
			return g, true
		}
	}
	return org.Goal{}, false
}

// goalsWhere says where a goal lives, for refusals and the log.
func goalsWhere(team string) string {
	if team == "" {
		return "the company's"
	}
	return team + "'s"
}

// addGoal adds a goal (to a team, if TEAM says so) as actor.
func (r *Runner) addGoal(actor string, fields []field) (org.Goal, error) {
	var g org.Goal
	for _, f := range fields {
		if msg := r.applyGoalField(&g, f.name, f.value); msg != "" {
			return g, refuse(invalid, "%s", msg)
		}
	}
	if g.Text == "" {
		return g, refuse(invalid, "a goal needs words.")
	}
	if g.Serves != 0 && g.Team == "" {
		return g, refuse(invalid, "only a team's objective serves a company goal.")
	}
	if err := r.allowed(actor, canArrange, where(g.Team), "set goals"); err != nil {
		return g, err
	}
	g, err := r.store.AddGoal(context.Background(), g)
	if err != nil {
		return g, r.storeRefusal("the goal", err)
	}
	r.logChange(actor, "add goal", fmt.Sprintf("#%d", g.ID), g.Team, describeGoal(g))
	r.updateWelcome(false)
	return g, nil
}

// setGoal changes goal id as actor.
func (r *Runner) setGoal(actor string, id int64, fields []field) (org.Goal, error) {
	g, ok := r.goal(id)
	if !ok {
		return g, refuse(notFound, "there's no goal #%d.", id)
	}
	if err := r.allowed(actor, canArrange, where(g.Team), "change "+goalsWhere(g.Team)+" goals"); err != nil {
		return g, err
	}
	before := g
	for _, f := range fields {
		if msg := r.applyGoalField(&g, f.name, f.value); msg != "" {
			return g, refuse(invalid, "%s", msg)
		}
	}
	if g.Team != before.Team {
		if err := r.allowed(actor, canArrange, where(g.Team), "move goals"); err != nil {
			return g, err
		}
	}
	if g.Serves != 0 && g.Team == "" {
		return g, refuse(invalid, "only a team's objective serves a company goal.")
	}
	if err := r.store.UpdateGoal(context.Background(), g); err != nil {
		return g, r.storeRefusal(fmt.Sprintf("goal #%d", id), err)
	}
	r.logChange(actor, "set goal", fmt.Sprintf("#%d", g.ID), g.Team, describeGoal(g))
	r.updateWelcome(false)
	return g, nil
}

// dropGoal removes goal id as actor. Objectives that served it no longer do.
func (r *Runner) dropGoal(actor string, id int64) error {
	g, ok := r.goal(id)
	if !ok {
		return refuse(notFound, "there's no goal #%d.", id)
	}
	if err := r.allowed(actor, canArrange, where(g.Team), "drop "+goalsWhere(g.Team)+" goals"); err != nil {
		return err
	}
	ctx := context.Background()
	if err := r.store.DropGoal(ctx, id); err != nil {
		return r.storeRefusal(fmt.Sprintf("goal #%d", id), err)
	}
	goals, _ := r.store.Goals(ctx)
	for _, o := range goals {
		if o.Serves == id {
			o.Serves = 0
			r.store.UpdateGoal(ctx, o)
		}
	}
	r.logChange(actor, "drop goal", fmt.Sprintf("#%d", id), g.Team, g.Text)
	r.updateWelcome(false)
	return nil
}

// describeGoal is a goal in a line: "#1 Grow to 100 weekly customers (Weekly
// active customers: 55 of 100)", and for an objective ", serving #1".
func describeGoal(g org.Goal) string {
	s := fmt.Sprintf("#%d %s", g.ID, g.Text)
	if m := describeMeasure(g); m != "" {
		s += " (" + m + ")"
	}
	if g.Serves != 0 {
		s += fmt.Sprintf(", serving #%d", g.Serves)
	}
	return s
}

func describeMeasure(g org.Goal) string {
	var s string
	switch {
	case g.Current != "" && g.Target != "":
		s = g.Current + " of " + g.Target
	case g.Target != "":
		s = "target " + g.Target
	case g.Current != "":
		s = "now " + g.Current
	}
	switch {
	case g.Measure != "" && s != "":
		return g.Measure + ": " + s
	case g.Measure != "":
		return g.Measure
	}
	return s
}

// guidingLights is what an agent is told the company and its team are
// aiming for: the company's description and goals, the focus of its team
// and the teams it's inside, and its own team's objectives.
func (r *Runner) guidingLights(team string) string {
	ctx := context.Background()
	var s strings.Builder
	if d, _ := r.store.Company(ctx); d != "" {
		fmt.Fprintf(&s, "About the company: %s\n", d)
	}
	goals, _ := r.store.Goals(ctx)
	var company, own []org.Goal
	for _, g := range goals {
		switch {
		case g.Team == "":
			company = append(company, g)
		case strings.EqualFold(g.Team, team):
			own = append(own, g)
		}
	}
	if len(company) > 0 {
		s.WriteString("The company's goals:\n")
		for _, g := range company {
			s.WriteString("- " + describeGoal(g) + "\n")
		}
	}
	seen := map[string]bool{}
	for cur := team; cur != "" && !seen[strings.ToLower(cur)]; cur = r.parentOf(cur) {
		seen[strings.ToLower(cur)] = true
		if t, err := r.store.Team(ctx, cur); err == nil && t.Focus != "" {
			fmt.Fprintf(&s, "%s's focus: %s\n", t.Name, t.Focus)
		}
	}
	if len(own) > 0 {
		fmt.Fprintf(&s, "%s's objectives:\n", own[0].Team)
		for _, g := range own {
			s.WriteString("- " + describeGoal(g) + "\n")
		}
	}
	return s.String()
}

// setFocusTopic makes a team's focus its channel's topic.
func (r *Runner) setFocusTopic(t org.Team) {
	if t.Focus == "" || t.Channel == "" || r.guildConn == nil {
		return
	}
	r.guildConn.Send("TOPIC", t.Channel, t.Focus)
}

// GuildServ's COMPANY, GOALS, and GOAL.

func (r *Runner) guildCompany(from, actor string, args []string) {
	if len(args) >= 2 && strings.EqualFold(args[0], "DESCRIBE") {
		description := strings.Join(args[1:], " ")
		if err := r.setCompany(actor, description); err != nil {
			r.guildReply(from, err.Error())
			return
		}
		r.guildReply(from, truncate("the company: "+description, maxLineBytes))
		return
	}
	if len(args) > 0 {
		r.guildReply(from, `usage: COMPANY, or COMPANY DESCRIBE "what the company does, for whom, and how".`)
		return
	}
	d, _ := r.store.Company(context.Background())
	if d == "" {
		d = `no description yet. COMPANY DESCRIBE "..." sets one.`
	}
	r.guildReply(from, truncate("The company: "+d, maxLineBytes))
	r.guildGoals(from, nil)
}

func (r *Runner) guildGoals(from string, args []string) {
	team := strings.Join(args, " ")
	goals, _ := r.store.Goals(context.Background())
	shown := 0
	for _, g := range goals {
		if team != "" && !strings.EqualFold(g.Team, team) {
			continue
		}
		line := describeGoal(g)
		if g.Team != "" {
			line = g.Team + ": " + line
		}
		r.guildReply(from, truncate(line, maxLineBytes))
		shown++
	}
	if shown == 0 {
		r.guildReply(from, `no goals yet. GOAL ADD "..." [TEAM name] [MEASURE "..." TARGET x CURRENT y] adds one.`)
	}
}

func (r *Runner) guildGoal(from, actor string, args []string) {
	usage := `usage: GOAL ADD "text" [TEAM name] [MEASURE "..."] [TARGET x] [CURRENT y] [SERVES n], GOAL SET <n> TEXT|TEAM|MEASURE|TARGET|CURRENT|SERVES <value>, or GOAL DROP <n>.`
	if len(args) < 2 {
		r.guildReply(from, usage)
		return
	}
	switch strings.ToUpper(args[0]) {
	case "ADD":
		fields, msg := pairs(args[2:])
		if msg != "" {
			r.guildReply(from, msg)
			return
		}
		g, err := r.addGoal(actor, append([]field{{"TEXT", args[1]}}, fields...))
		if err != nil {
			r.guildReply(from, err.Error())
			return
		}
		r.guildReply(from, truncate(fmt.Sprintf("added %s goal %s.", goalsWhere(g.Team), describeGoal(g)), maxLineBytes))
	case "SET":
		id, err := strconv.ParseInt(strings.TrimPrefix(args[1], "#"), 10, 64)
		if err != nil || len(args) < 4 {
			r.guildReply(from, usage)
			return
		}
		g, err := r.setGoal(actor, id, []field{{args[2], strings.Join(args[3:], " ")}})
		if err != nil {
			r.guildReply(from, err.Error())
			return
		}
		r.guildReply(from, truncate("updated goal "+describeGoal(g)+".", maxLineBytes))
	case "DROP":
		id, err := strconv.ParseInt(strings.TrimPrefix(args[1], "#"), 10, 64)
		if err != nil {
			r.guildReply(from, usage)
			return
		}
		if err := r.dropGoal(actor, id); err != nil {
			r.guildReply(from, err.Error())
			return
		}
		r.guildReply(from, fmt.Sprintf("dropped goal #%d.", id))
	default:
		r.guildReply(from, usage)
	}
}

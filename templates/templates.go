// Package templates lists what a new network can start from. Each one is a
// directory here (a guildnet.yaml and its personas) that the runner's image
// carries; see internal/config for how a network's runner fills one in.
package templates

// Template is one starting point for a network.
type Template struct {
	Name  string // its directory, and what the host and runner call it
	Title string // what the create page calls it
	About string // one line on what it sets up
	// Agents are its agents' nicks, which the owner's nick mustn't take.
	Agents []string
}

// All is every template, in the order the create page offers them.
var All = []Template{
	{Name: "solo", Title: "Solo developer", About: "One generalist engineer agent who works with you.", Agents: []string{"sam"}},
	{Name: "team", Title: "Small dev team", About: "An engineer, a code reviewer, and a product manager.", Agents: []string{"kit", "rey", "pia"}},
	{Name: "saas", Title: "SaaS startup", About: "Leadership, engineering, support, and marketing: a chief of staff, an engineer and a reviewer, a support agent, and a writer.", Agents: []string{"max", "ivy", "otto", "pip", "cora"}},
	{Name: "agency", Title: "Agency", About: "Client work, client care, and the books: an engineer, a designer, an account manager, and a bookkeeper.", Agents: []string{"juno", "wren", "remy", "bea"}},
	{Name: "smallbiz", Title: "Small business", About: "Help for customers, getting the word out, and the books: a support agent, a writer, a social media helper, and a bookkeeper.", Agents: []string{"pip", "cora", "lulu", "bea"}},
	{Name: "opensource", Title: "Open-source project", About: "An agent that triages issues and one that keeps the docs up to date.", Agents: []string{"tess", "dot"}},
}

// Known says whether name is a template.
func Known(name string) bool {
	_, ok := Find(name)
	return ok
}

// Find is the template called name.
func Find(name string) (Template, bool) {
	for _, t := range All {
		if t.Name == name {
			return t, true
		}
	}
	return Template{}, false
}

// SPDX-License-Identifier: AGPL-3.0-only

// Package connectors is GuildNet's connectors: services a company's agents
// use as tools (Linear, Notion, Sentry, Atlassian), each an MCP server the
// network's owner connects once, with OAuth. The platform does the
// connecting and keeps the tokens; the network's runner calls the servers
// with short-lived access tokens from the platform's broker, so agents only
// ever see tools.
package connectors

import "strings"

// Connector is a service agents can be given.
type Connector struct {
	ID    string // e.g. "linear": in tool names and addresses
	Title string
	About string // what it's for, in a few words
	URL   string // its MCP server
	// Issuer is its authorization server, for a server that doesn't say
	// where that is (it publishes no protected resource metadata); "" means
	// it says.
	Issuer string
	// Reads are its tools that only read, which agents may use without
	// asking; the rest ask a person first until someone says otherwise. A
	// name ending in * is a prefix: get_* is every tool whose name starts
	// get_.
	Reads []string
	// Runs are its tools that run another of its tools, named in this
	// argument (Sentry's execute_sentry_tool: "name"), with its input in
	// "arguments". By default a call runs when the tool it names only
	// reads, and asks first otherwise.
	Runs map[string]string
}

// ReadOnly says whether tool only reads, as the catalogue says.
func (c Connector) ReadOnly(tool string) bool {
	for _, r := range c.Reads {
		if prefix, ok := strings.CutSuffix(r, "*"); ok && strings.HasPrefix(tool, prefix) || r == tool {
			return true
		}
	}
	return false
}

// Catalogue is the connectors GuildNet offers. Each works with the MCP
// authorization spec as it is, with no app of ours to review.
var Catalogue = []Connector{
	// Linear's 68 tools, as listed on 2026-09-30: get_*, list_* and
	// search_documentation only read; save_*, delete_*, create_*, share_*
	// and the rest change things.
	{ID: "linear", Title: "Linear", About: "Issues, projects and cycles", URL: "https://mcp.linear.app/mcp",
		Reads: []string{"get_*", "list_*", "search_documentation", "extract_images"}},
	{ID: "notion", Title: "Notion", About: "Pages and databases", URL: "https://mcp.notion.com/mcp"},
	// Sentry's, as listed on 2026-09-30: find_*, search_*, get_* and whoami
	// only read; update_*, create_*, analyze_issue_with_seer (starts Seer's
	// paid analysis) and the rest change things. Most of its tools sit
	// behind execute_sentry_tool, which runs the one its name says with
	// its arguments.
	{ID: "sentry", Title: "Sentry", About: "Errors, issues and releases", URL: "https://mcp.sentry.dev/mcp",
		Reads: []string{"find_*", "search_*", "get_*", "whoami"},
		Runs:  map[string]string{"execute_sentry_tool": "name"}},
	// Atlassian's server publishes no resource metadata.
	{ID: "atlassian", Title: "Atlassian", About: "Jira and Confluence", URL: "https://mcp.atlassian.com/v1/mcp", Issuer: "https://mcp.atlassian.com"},
}

// Find is the connector in list with this ID.
func Find(list []Connector, id string) (Connector, bool) {
	for _, c := range list {
		if strings.EqualFold(c.ID, id) {
			return c, true
		}
	}
	return Connector{}, false
}

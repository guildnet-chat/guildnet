// SPDX-License-Identifier: AGPL-3.0-only

package managed

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/guildnet-chat/guildnet/config"
	"github.com/guildnet-chat/guildnet/internal/agent"
)

// SDKDesks is Desks backed by the Managed Agents API.
type SDKDesks struct {
	keyMu       sync.RWMutex // guards client, which changes with the key
	client      anthropic.Client
	environment string
	network     string // tags agent configs, so several networks can share an account
	budgetCents int64  // each desk's lifetime spending cap
	repos       Repos

	mu     sync.Mutex
	tokens map[string]string // session -> the repo token it was last given
}

// Repos are the repositories checked out, read-only, in an agent's
// workspace (none is fine). The token is short-lived; desks get a fresh one
// when it changes.
type Repos interface {
	Checkout(ctx context.Context, p config.Persona) (urls []string, token string, err error)
}

// SDKOptions configure SDKDesks.
type SDKOptions struct {
	Network     string // e.g. "guildnet"
	BudgetCents int64  // lifetime cap per desk; 0 means $1
	// AllowedHosts are reachable from desks' sandboxes, beyond package managers.
	AllowedHosts []string
	Repos        Repos // may be nil
	// APIKey is the Anthropic key; "" means ANTHROPIC_API_KEY.
	APIKey string
}

func newClient(key string) anthropic.Client {
	if key == "" {
		return anthropic.NewClient()
	}
	return anthropic.NewClient(option.WithAPIKey(key))
}

// api is the client, with the key there is now.
func (d *SDKDesks) api() *anthropic.Client {
	d.keyMu.RLock()
	defer d.keyMu.RUnlock()
	c := d.client
	return &c
}

// UseKey has desks use key from their next call on (the network's key was
// replaced).
func (d *SDKDesks) UseKey(key string) {
	c := newClient(key)
	d.keyMu.Lock()
	defer d.keyMu.Unlock()
	d.client = c
}

// NewSDKDesks finds or creates the network's environment.
func NewSDKDesks(ctx context.Context, opts SDKOptions) (*SDKDesks, error) {
	d := &SDKDesks{client: newClient(opts.APIKey), network: opts.Network, budgetCents: opts.BudgetCents,
		repos: opts.Repos, tokens: map[string]string{}}
	if d.budgetCents <= 0 {
		d.budgetCents = 100
	}
	name := "guildnet-" + opts.Network
	cloud := &anthropic.BetaCloudConfigParams{
		Networking: anthropic.BetaCloudConfigParamsNetworkingUnion{
			OfLimited: &anthropic.BetaLimitedNetworkParams{
				AllowPackageManagers: anthropic.Bool(true),
				AllowMCPServers:      anthropic.Bool(true),
				AllowedHosts:         opts.AllowedHosts,
			},
		},
	}
	iter := d.api().Beta.Environments.ListAutoPaging(ctx, anthropic.BetaEnvironmentListParams{})
	for iter.Next() {
		if e := iter.Current(); e.Name == name && e.ArchivedAt == "" {
			// Keep its settings in step with ours, e.g. newly allowed hosts.
			if _, err := d.api().Beta.Environments.Update(ctx, e.ID, anthropic.BetaEnvironmentUpdateParams{
				Config: anthropic.BetaEnvironmentUpdateParamsConfigUnion{OfCloud: cloud},
			}); err != nil {
				return nil, fmt.Errorf("updating environment %s: %w", name, err)
			}
			d.environment = e.ID
			return d, nil
		}
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("listing environments: %w", err)
	}
	env, err := d.api().Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
		Name:   name,
		Config: anthropic.BetaEnvironmentNewParamsConfigUnion{OfCloud: cloud},
	})
	if err != nil {
		return nil, fmt.Errorf("creating environment %s: %w", name, err)
	}
	d.environment = env.ID
	return d, nil
}

// tools are the built-in toolset (bash, files, web) plus our custom tools,
// which the runner carries out.
func (d *SDKDesks) tools(custom []agent.Tool) []anthropic.BetaAgentNewParamsToolUnion {
	out := []anthropic.BetaAgentNewParamsToolUnion{{
		OfAgentToolset20260401: &anthropic.BetaManagedAgentsAgentToolset20260401Params{
			Type: anthropic.BetaManagedAgentsAgentToolset20260401ParamsTypeAgentToolset20260401,
		},
	}}
	for _, t := range custom {
		out = append(out, anthropic.BetaAgentNewParamsToolUnion{OfCustom: &anthropic.BetaManagedAgentsCustomToolParams{
			Type:        anthropic.BetaManagedAgentsCustomToolParamsTypeCustom,
			Name:        t.Name,
			Description: t.Description,
			InputSchema: anthropic.BetaManagedAgentsCustomToolInputSchemaParam{Properties: t.Properties, Required: t.Required},
		}})
	}
	return out
}

func (d *SDKDesks) updateTools(custom []agent.Tool) []anthropic.BetaAgentUpdateParamsToolUnion {
	var out []anthropic.BetaAgentUpdateParamsToolUnion
	for _, t := range d.tools(custom) {
		out = append(out, anthropic.BetaAgentUpdateParamsToolUnion{OfAgentToolset20260401: t.OfAgentToolset20260401, OfCustom: t.OfCustom})
	}
	return out
}

// EnsureAgent finds the agent's config by its GuildNet tags, updating it if
// the prompt or model changed, or creates it.
func (d *SDKDesks) EnsureAgent(ctx context.Context, p config.Persona, system string, tools []agent.Tool) (string, int64, error) {
	hash := Hash(p, system, tools)
	meta := map[string]string{"guildnet_network": d.network, "guildnet_nick": strings.ToLower(p.Nick), "guildnet_hash": hash}
	model := anthropic.BetaManagedAgentsModelConfigParams{ID: anthropic.BetaManagedAgentsModel(p.Model)}
	name := fmt.Sprintf("%s (%s) - %s", p.Name, p.Nick, d.network)

	iter := d.api().Beta.Agents.ListAutoPaging(ctx, anthropic.BetaAgentListParams{})
	for iter.Next() {
		a := iter.Current()
		if !a.ArchivedAt.IsZero() || a.Metadata["guildnet_network"] != d.network || a.Metadata["guildnet_nick"] != strings.ToLower(p.Nick) {
			continue
		}
		if a.Metadata["guildnet_hash"] == hash {
			return a.ID, a.Version, nil
		}
		updated, err := d.api().Beta.Agents.Update(ctx, a.ID, anthropic.BetaAgentUpdateParams{
			Version: anthropic.Int(a.Version), Name: anthropic.String(name), Model: model,
			System: anthropic.String(system), Metadata: meta, Tools: d.updateTools(tools),
		})
		if err != nil {
			return "", 0, err
		}
		return updated.ID, updated.Version, nil
	}
	if err := iter.Err(); err != nil {
		return "", 0, err
	}
	created, err := d.api().Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
		Name: name, Model: model, System: anthropic.String(system), Tools: d.tools(tools), Metadata: meta,
	})
	if err != nil {
		return "", 0, err
	}
	return created.ID, created.Version, nil
}

// NewDesk starts the agent's long-lived session with a lifetime budget.
func (d *SDKDesks) NewDesk(ctx context.Context, p config.Persona, agentID string, version int64) (string, error) {
	nick := p.Nick
	var resources []anthropic.BetaSessionNewParamsResourceUnion
	var token string
	if d.repos != nil {
		urls, t, err := d.repos.Checkout(ctx, p)
		if err != nil {
			return "", err
		}
		token = t
		for _, url := range urls {
			resources = append(resources, anthropic.BetaSessionNewParamsResourceUnion{
				OfGitHubRepository: &anthropic.BetaManagedAgentsGitHubRepositoryResourceParams{
					Type:               anthropic.BetaManagedAgentsGitHubRepositoryResourceParamsTypeGitHubRepository,
					URL:                url,
					AuthorizationToken: token,
				},
			})
		}
	}
	s, err := d.api().Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Resources: resources,
		Agent: anthropic.BetaSessionNewParamsAgentUnion{
			OfBetaManagedAgentsAgents: &anthropic.BetaManagedAgentsAgentParams{
				Type: anthropic.BetaManagedAgentsAgentParamsTypeAgent, ID: agentID, Version: anthropic.Int(version),
			},
		},
		EnvironmentID: d.environment,
		Title:         anthropic.String(nick + "'s desk"),
		Metadata:      map[string]string{"guildnet_network": d.network, "guildnet_nick": strings.ToLower(nick)},
		Budget: anthropic.BetaManagedAgentsBudgetLimitParam{
			Type:        anthropic.BetaManagedAgentsBudgetLimitTypeLimit,
			MaxListCost: anthropic.BetaMonetaryAmountParam{Amount: strconv.FormatInt(d.budgetCents, 10), Currency: anthropic.BetaCurrencyUsd},
		},
	})
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	d.tokens[s.ID] = token
	d.mu.Unlock()
	return s.ID, nil
}

// freshRepos gives the desk's checked-out repos the current read-only token,
// if it has changed since the desk last got one (they last an hour).
func (d *SDKDesks) freshRepos(ctx context.Context, p config.Persona, session string) error {
	if d.repos == nil {
		return nil
	}
	_, token, err := d.repos.Checkout(ctx, p)
	if err != nil || token == "" {
		return err
	}
	d.mu.Lock()
	current := d.tokens[session] == token
	d.mu.Unlock()
	if current {
		return nil
	}
	iter := d.api().Beta.Sessions.Resources.ListAutoPaging(ctx, session, anthropic.BetaSessionResourceListParams{})
	for iter.Next() {
		if r := iter.Current(); r.Type == "github_repository" {
			if _, err := d.api().Beta.Sessions.Resources.Update(ctx, r.ID, anthropic.BetaSessionResourceUpdateParams{
				SessionID: session, AuthorizationToken: token,
			}); err != nil {
				return fmt.Errorf("refreshing the repo token: %w", err)
			}
		}
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("listing the desk's repos: %w", err)
	}
	d.mu.Lock()
	d.tokens[session] = token
	d.mu.Unlock()
	return nil
}

// Send wakes the desk and collects its events until it stops.
func (d *SDKDesks) Send(ctx context.Context, p config.Persona, session, text string, live func(Event)) ([]Event, error) {
	return d.exchange(ctx, p, session, live, anthropic.BetaManagedAgentsEventParamsUnion{
		OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
			Type: anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage,
			Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{
				OfText: &anthropic.BetaManagedAgentsTextBlockParam{Type: anthropic.BetaManagedAgentsTextBlockTypeText, Text: text},
			}},
		},
	})
}

// Answer sends results for custom tools and decisions on built-in ones, and
// collects events until the desk stops.
func (d *SDKDesks) Answer(ctx context.Context, p config.Persona, session string, answers []Answer, live func(Event)) ([]Event, error) {
	var evs []anthropic.BetaManagedAgentsEventParamsUnion
	for _, a := range answers {
		if a.Result {
			evs = append(evs, anthropic.BetaManagedAgentsEventParamsUnion{OfUserCustomToolResult: &anthropic.BetaManagedAgentsUserCustomToolResultEventParams{
				Type:            anthropic.BetaManagedAgentsUserCustomToolResultEventParamsTypeUserCustomToolResult,
				CustomToolUseID: a.EventID,
				IsError:         anthropic.Bool(a.IsError),
				Content: []anthropic.BetaManagedAgentsUserCustomToolResultEventParamsContentUnion{{
					OfText: &anthropic.BetaManagedAgentsTextBlockParam{Type: anthropic.BetaManagedAgentsTextBlockTypeText, Text: a.Text},
				}},
			}})
			continue
		}
		c := &anthropic.BetaManagedAgentsUserToolConfirmationEventParams{
			Type:      anthropic.BetaManagedAgentsUserToolConfirmationEventParamsTypeUserToolConfirmation,
			ToolUseID: a.EventID,
			Result:    anthropic.BetaManagedAgentsUserToolConfirmationEventParamsResultAllow,
		}
		if !a.Allow {
			c.Result = anthropic.BetaManagedAgentsUserToolConfirmationEventParamsResultDeny
			if a.Text != "" {
				c.DenyMessage = anthropic.String(a.Text)
			}
		}
		evs = append(evs, anthropic.BetaManagedAgentsEventParamsUnion{OfUserToolConfirmation: c})
	}
	return d.exchange(ctx, p, session, live, evs...)
}

// Archive retires an agent config.
func (d *SDKDesks) Archive(ctx context.Context, agentID string) error {
	_, err := d.api().Beta.Agents.Archive(ctx, agentID, anthropic.BetaAgentArchiveParams{})
	return err
}

// exchange opens the desk's stream first (it only delivers what happens
// after it opens), sends our events, and collects events until the desk goes
// idle after them, stops for good, or reports an error.
func (d *SDKDesks) exchange(ctx context.Context, p config.Persona, session string, live func(Event), evs ...anthropic.BetaManagedAgentsEventParamsUnion) ([]Event, error) {
	if err := d.freshRepos(ctx, p, session); err != nil {
		return nil, err
	}
	stream := d.api().Beta.Sessions.Events.StreamEvents(ctx, session, anthropic.BetaSessionEventStreamParams{})
	defer stream.Close()
	if _, err := d.api().Beta.Sessions.Events.Send(ctx, session, anthropic.BetaSessionEventSendParams{
		Events: evs,
	}); err != nil {
		return nil, err
	}
	var out []Event
	emit := func(e Event) {
		out = append(out, e)
		if live != nil {
			live(e)
		}
	}
	started := false // ignore an idle that was already pending before our event
	for stream.Next() {
		switch e := stream.Current().AsAny().(type) {
		case anthropic.BetaManagedAgentsUserMessageEvent, anthropic.BetaManagedAgentsUserToolConfirmationEvent,
			anthropic.BetaManagedAgentsUserCustomToolResultEvent, anthropic.BetaManagedAgentsSessionStatusRunningEvent:
			started = true
		case anthropic.BetaManagedAgentsAgentMessageEvent:
			started = true
			var text strings.Builder
			for _, block := range e.Content {
				text.WriteString(block.Text)
			}
			emit(Event{Kind: Message, Text: text.String()})
		case anthropic.BetaManagedAgentsAgentToolUseEvent:
			started = true
			emit(Event{Kind: ToolUse, ID: e.ID, Tool: e.Name, Input: e.Input,
				Ask: e.EvaluatedPermission == anthropic.BetaManagedAgentsAgentEvaluatedPermissionAsk})
		case anthropic.BetaManagedAgentsAgentCustomToolUseEvent:
			started = true
			emit(Event{Kind: ToolUse, ID: e.ID, Tool: e.Name, Custom: true, Input: e.Input})
		case anthropic.BetaManagedAgentsAgentMCPToolUseEvent:
			started = true
			emit(Event{Kind: ToolUse, ID: e.ID, Tool: e.Name, MCPServer: e.MCPServerName, Input: e.Input,
				Ask: e.EvaluatedPermission == anthropic.BetaManagedAgentsAgentEvaluatedPermissionAsk})
		case anthropic.BetaManagedAgentsSessionUsageEvent:
			cents, _ := strconv.ParseInt(e.Usage.ListCost.Amount, 10, 64)
			emit(Event{Kind: Usage, CostCents: cents})
		case anthropic.BetaManagedAgentsSessionErrorEvent:
			emit(Event{Kind: Error, Text: e.Error.Message})
			return out, nil
		case anthropic.BetaManagedAgentsSessionStatusTerminatedEvent:
			emit(Event{Kind: Error, Text: "the desk session ended"})
			return out, nil
		case anthropic.BetaManagedAgentsSessionStatusIdleEvent:
			stop := e.StopReason.Type
			if !started && stop == "end_turn" {
				continue
			}
			emit(Event{Kind: Idle, Stop: stop, Waiting: e.StopReason.EventIDs})
			out = append(out, d.usage(ctx, session)...)
			return out, nil
		}
	}
	if err := stream.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// usage reads the desk's cumulative cost after a turn.
func (d *SDKDesks) usage(ctx context.Context, session string) []Event {
	s, err := d.api().Beta.Sessions.Get(ctx, session, anthropic.BetaSessionGetParams{})
	if err != nil {
		return nil
	}
	cents, err := strconv.ParseInt(s.Usage.ListCost.Amount, 10, 64)
	if err != nil {
		return nil
	}
	return []Event{{Kind: Usage, CostCents: cents}}
}

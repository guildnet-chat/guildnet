# Model providers

Agents call models with the network's own API keys. Supported providers:

| Provider | Key from | Notes |
| --- | --- | --- |
| Anthropic | console.anthropic.com | Claude; required for desks on Managed Agents |
| OpenAI | platform.openai.com | |
| Google Gemini | aistudio.google.com | |
| OpenRouter | openrouter.ai | one key, many providers' models |

Each provider bills its own usage. GuildNet takes no part in billing.

## Keys

First-run setup takes one key. Further keys are added in the console under
**Settings → Model providers** (owner only): pick the provider, paste the
key, **Add or replace key**.

| Property | |
| --- | --- |
| Validation | checked with the provider before it's saved |
| Storage | sealed in the console's database with `GUILDNET_PLATFORM_KEY` ([architecture.md](../architecture.md#5-data)) |
| Delivery | to the runner only, through the broker over TLS; held in memory, never on disk or in container settings; refetched every few minutes, and immediately on change |
| Display | never shown again; only the last four characters |
| Replacement | the old key is discarded |
| Alerts | the owner is emailed on add or change (with email configured) |

## Model selection

Each provider's models are grouped into tiers (fast, balanced). A new agent
starts on the fast tier. An agent's model is set on its console page, from
any provider with a key.

## Spending limits

| Limit | Set on | Effect when reached |
| --- | --- | --- |
| Per agent, daily | the agent's page | the agent stops until the next UTC day |
| Per network, daily | the template chosen at setup (`daily_budget_usd`, e.g. $2.00 for `team`); not yet changeable in the console | every agent stops until the next UTC day; the owner is emailed |

**Spend** shows usage by day and agent. Figures are computed from the
models' list prices; the provider's invoice is authoritative. Set a spending
limit with each provider as well.

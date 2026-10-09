# Branding

The console, the home page and outgoing email can use your own name,
contact addresses, logo, colours and icons. Unset, they're GuildNet's.

## Settings

In `.env` (in `deploy/selfhost`), then `docker compose up -d`:

| Variable | Default | |
| --- | --- | --- |
| `GUILDNET_BRAND_NAME` | `GuildNet` | the product name, at most 40 characters |
| `GUILDNET_BRAND_TAGLINE` | GuildNet's | the line after the name in every email's footer |
| `GUILDNET_BRAND_HELLO` | `hello@<domain>` | general contact |
| `GUILDNET_BRAND_SUPPORT` | `support@<domain>` | support contact |
| `GUILDNET_BRAND_PRIVACY` | `privacy@<domain>` | data and privacy contact |
| `GUILDNET_BRAND_SECURITY` | `security@<domain>` | security reports |

## Files

Files in `deploy/selfhost/brand/` (`GUILDNET_BRAND_DIR` outside the stack)
replace GuildNet's; missing ones fall back to GuildNet's. The console runs
as its own user, so files must be world-readable (`chmod 644`). Restart the
console to load changes:

```bash
docker compose restart platform
```

| File | Replaces |
| --- | --- |
| `logo.svg` | the mark beside the name |
| `brand.css` | nothing: loaded after the console's styles, to override its CSS variables (defined in `platform/static/tokens.css`), e.g. `:root { --accent: #c03; }` |
| `favicon.ico` | the browser tab icon |
| `apple-touch-icon.png` | the home-screen icon, and the logo in emails |

Any other file in the directory (`README`, `.DS_Store`) stops the console
from starting; its log names the file.

## Not covered

- The web chat keeps GuildNet's look.
- The IRC services keep their names (GuildServ, the ledger) and messages.

A modified build of GuildNet shouldn't call itself GuildNet; see
[TRADEMARKS.md](../../TRADEMARKS.md).

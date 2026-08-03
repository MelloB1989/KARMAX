# Multiple KARMAX instances on one host

One KARMAX per user, sharing a single machine and a single OS account.

```bash
install -m755 karmax-tenant ~/.local/bin/

karmax-tenant create alice     # dirs, ports, secrets, systemd units
karmax-tenant login  alice     # scan their WhatsApp QR, sign into their Google
karmax-tenant start  alice
karmax-tenant list             # every tenant, its ports and status
```

## What each tenant gets

| Resource | Path / value |
|---|---|
| KARMAX state | `~/.karmax/tenants/<name>` (`KARMAX_DATA_DIR`) |
| WhatsApp session | `~/.wacli/tenants/<name>` (`WACLI_HOME`) |
| Google credentials | `~/.config/gws/tenants/<name>` (`GOOGLE_WORKSPACE_CLI_CONFIG_DIR`) |
| Env file | `~/.config/karmax/tenants/<name>.env` (mode 600) |
| KARMAX API | `9200 + index` |
| Webhook receiver | `9300 + index` |
| wacli API | `8800 + index` |
| API token / webhook secret | generated per tenant, never shared |

Indices are reused when a tenant is removed, so ports stay compact.

## Google, specifically

`gws` stores credentials in the OS keyring by default — which is **per OS user**,
so tenants would overwrite each other. Each tenant therefore gets
`GOOGLE_WORKSPACE_CLI_KEYRING_BACKEND=file` plus its own config dir, keeping the
tokens separate on disk.

## Isolation — read this

All tenants run as the **same OS user**. Separation is by directory and 0700
permissions, *not* enforced by the kernel: any process running as this user can
read every tenant's WhatsApp database and Google tokens.

That is fine for your own personas or a trusted team. For untrusted or paying
customers, give each tenant its own OS user:

```bash
sudo useradd -m karmax-alice
sudo loginctl enable-linger karmax-alice
# copy the same unit files into ~karmax-alice/.config/systemd/user/
```

The units and env layout are written so this works unchanged — only the account
they run under differs.

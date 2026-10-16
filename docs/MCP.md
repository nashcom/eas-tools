# eas-mcp: the mailbox for AI clients

> **Experimental.** `eas-mcp` is a first addition: the tools, the options and the defaults can still change, and sending mail has been tried
> against the mock server only.

`eas-mcp` is an [MCP](https://modelcontextprotocol.io) server. An AI client such as Claude Code uses its tools to look at a mailbox through
ActiveSync, and, when you allow it, to send mail. It serves HTTP (the default, for a container) or talks over stdin and stdout when the client
starts it. It is a thin layer on the same code as `eas-client`
([CLIENT.md](CLIENT.md)): the accounts, the sessions and the checks are those of the command line and the REST API.

Contents: [Quick start](#quick-start) | [The tools](#the-tools) | [What eas-client has, and the tool for it](#what-eas-client-has-and-the-tool-for-it) | [Configuration](#configuration) | [Sending mail](#sending-mail) | [Login with OIDC](#login-with-oidc) | [In a container: the transport http](#in-a-container-the-transport-http) | [Protocol versions and sessions](#protocol-versions-and-sessions) | [Logging and the audit log](#logging-and-the-audit-log) | [Security](#security)

## Quick start

```bash
go build -o eas-mcp ./cmd/eas-mcp
```

The account goes into the config file of `eas-client` (see [CLIENT.md](CLIENT.md)), with the password in a file or in the environment. Then
register the program with Claude Code:

```bash
claude mcp add --transport stdio eas -- /absolute/path/to/eas-mcp -transport stdio -config /absolute/path/to/client.json
```

`claude mcp list` shows it, and `/mcp` in Claude Code shows the connection and the tools. Ask, for example: "List the folders of my mailbox",
or "Show me the latest mails in the inbox".

## The tools

| Tool                       | What it does                                                                                                                                                                                                             |
| -------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `accounts`                 | the configured accounts and whether sending is allowed; it does not connect                                                                                                                                              |
| `server_info`              | the server (product, protocol versions, commands), the device that is presented, the number of folders                                                                                                                   |
| `list_folders`             | the folders with id, name and type                                                                                                                                                                                       |
| `list_messages`            | the mails of a folder, newest first, in a date window (`since`), with `limit` and an optional `preview`                                                                                                                  |
| `get_message`              | one mail with its whole text                                                                                                                                                                                             |
| `get_message_eml`          | the whole message as EML (RFC 822): every header line, the MIME structure, the body and the attachments as they are encoded; `headers_only` returns the header lines alone, `max_bytes` limits the size (default 200000) |
| `send_message`             | send a mail (`to`, `cc`, `subject`, `body`): only with `-send`, and only to the domains of `-send-domains`                                                                                                               |
| `reply_message`            | reply to a mail (`id`, `body`, `all`): only with `-send`, and only to the domains of `-send-domains`                                                                                                                     |
| `forward_message`          | forward a mail (`id`, `to`, `cc`, `body`): only with `-send`, and only to the domains of `-send-domains`                                                                                                                 |
| `mark_read`, `mark_unread` | set the read flag of a mail (`id`): only with `-write`                                                                                                                                                                   |
| `move_message`             | move a mail to another folder (`id`, `destination`): only with `-write`                                                                                                                                                  |
| `delete_message`           | delete a mail, it goes to Deleted Items (`id`): only with `-write`                                                                                                                                                       |
| `list_events`              | the events of the calendar by start time, from now on unless `from` says another start; `to`, `since` and `limit` shape the window                                                                                       |
| `get_event`                | one event with all details: attendees and their answers, reminder, repeat, description                                                                                                                                   |
| `create_event`             | create an event (`subject`, `start`, `end` or `duration`, `location`, `attendees`, ...): only with `-write`; with attendees also `-send`, and only to `-send-domains`                                                    |
| `delete_event`             | delete an event; the cancellation of a meeting that you organize is mailed to its attendees: only with `-write` (and `-send` for a meeting)                                                                              |
| `list_contacts`            | the contacts of the address book, or those that match `search`                                                                                                                                                           |
| `get_contact`              | one contact with all details (no photo)                                                                                                                                                                                  |
| `get_event_ics`            | one event as an iCalendar file (`.ics`) in plain text, the raw form for another calendar; `max_bytes` limits the size                                                                                                    |
| `get_contact_vcard`        | one contact as a vCard file (`.vcf`) in plain text; `no_photo` leaves out the photo, `max_bytes` limits the size                                                                                                         |
| `export_contacts`          | all contacts, or those that match `search`, as one vCard file in plain text; `no_photo` and `max_bytes` as above                                                                                                         |

The tools that send mail (`send_message`, `reply_message`, `forward_message`) are there only with `-send`, and the tools that change a mailbox
(`mark_read`, `mark_unread`, `move_message`, `delete_message`, `create_event`, `delete_event`) only with `-write`: two switches, as in `eas-client`. A meeting
with attendees is both: creating it mails the invitations and deleting it mails the cancellation, so it needs `-send` too, and `-send-domains` limits the
attendees like any other recipient. Contacts can be read only. Every tool takes `account` when more than one account is configured. The answers are the JSON of the same operations of `eas-client`.

## What eas-client has, and the tool for it

Every command of `eas-client` has a tool, and every option of a command has an argument. What a tool answers is what the command prints.

| `eas-client`                                                           | Tool                                     | The answer                                                      |
| ---------------------------------------------------------------------- | ---------------------------------------- | --------------------------------------------------------------- |
| `accounts`                                                             | `accounts`                               | JSON                                                            |
| `info`                                                                 | `server_info`                            | JSON                                                            |
| `folders`                                                              | `list_folders`                           | JSON                                                            |
| `list FOLDER -limit -preview -since`                                   | `list_messages`                          | JSON                                                            |
| `get`                                                                  | `get_message`                            | JSON                                                            |
| `eml`                                                                  | `get_message_eml`                        | the message as plain text (`headers_only` for the header lines) |
| `send`, `-no-sent-copy`                                                | `send_message`, `no_sent_copy`           | JSON                                                            |
| `reply -all`                                                           | `reply_message`, `all`                   | JSON                                                            |
| `forward`                                                              | `forward_message`                        | JSON                                                            |
| `read`, `unread`                                                       | `mark_read`, `mark_unread`               | JSON                                                            |
| `move`, `delete`                                                       | `move_message`, `delete_message`         | JSON                                                            |
| `calendar list`                                                        | `list_events`                            | JSON                                                            |
| `calendar get`                                                         | `get_event`                              | JSON                                                            |
| `calendar ics`                                                         | `get_event_ics`                          | the iCalendar text                                              |
| `calendar create` (all options, `-attendee`, `-optional`, `-resource`) | `create_event` (`attendees` with `type`) | JSON                                                            |
| `calendar delete -no-cancel-mail`                                      | `delete_event`, `no_cancel_mail`         | JSON                                                            |
| `contacts list`                                                        | `list_contacts`                          | JSON                                                            |
| `contacts get`                                                         | `get_contact`                            | JSON                                                            |
| `contacts vcf`                                                         | `get_contact_vcard`                      | the vCard text                                                  |
| `contacts export`                                                      | `export_contacts`                        | the vCard text                                                  |
| `serve`                                                                | the transport http itself                |                                                                 |

**The forms of the answers.** A tool that answers with JSON gives it as text and as structured content (a client that knows the schema gets the
fields). A tool that answers with a file (the message, the event, the contact, the export) gives plain text only, cut at `max_bytes` (default
200000, at most 2000000) with a line that says how much there is. The options that only mean something on the command line (`-body-file`,
`-body-stdin`, the files and the standard input) are not there: the text is an argument.

**What neither has:** attachments (a mail can be read in its raw form with `get_message_eml`, where they are in encoded form), a reply with another
sender, and creating or changing a contact. The account, the time zone and the device are the configuration of the server, as with `eas-client`.

## Configuration

The configuration is that of `eas-client`. **Every option can be given in three ways**, in this order of precedence:

| Where            | How                                                                                              | Example for `-oidc-users`            |
| ---------------- | ------------------------------------------------------------------------------------------------ | ------------------------------------ |
| the command line | `-name value`                                                                                    | `-oidc-users '*@example.com'`        |
| the environment  | `EASCLIENT_` and the name in upper case, `_` for `-`                                             | `EASCLIENT_OIDC_USERS=*@example.com` |
| the config file  | `-config FILE` (or `EASCLIENT_CONFIG`); the key is the name with `_` for `-`; a list is an array | `"oidc_users": ["*@example.com"]`    |

The accounts are the list `accounts` of the file. A password and a token are the exceptions: they are not options, because the command line
is visible to every user of the machine. They are `EASCLIENT_PASSWORD` and `EASCLIENT_AUTH_TOKEN` (or the keys `password` and `auth_token`, or
the files `-password-file` and `-auth-token-file`). A test goes through all the options and checks the three ways, so a new option cannot be
left out. `eas-mcp -h` lists them.

An account of the file that names no password uses the variable `EASCLIENT_PASSWORD` (`-password-env` names another one). `-password-stdin` is
refused with the transport stdio, because stdin carries the protocol. The log is on stderr (`-verbose`, `-debug`, `-log-format`); stdout is only
for MCP.

## Sending mail

`send_message`, `reply_message` and `forward_message` exist only with **`-send`**, the same switch as `eas-client`. Without it the AI does not see
the tools, and the manager would refuse the send anyway. **`-write`** is the other switch: it adds the tools that change a mailbox (mark read
and unread, move, delete). It does not include sending.

**`-send-domains`** (`send_domains` in the file) limits where mail may go: `example.com` is that domain alone, `*.example.com` is every domain
below it; repeat the option or separate the values by commas. The check is on every recipient (To and Cc), and one that fails stops the mail.
It is in the same place for `eas-client`, its REST API and `eas-mcp`: reply and forward are checked too (a reply goes to the sender of the
mail, and with all to the others). Without the option there is no limit, and the start says so in the log.

## Login with OIDC

A client can log in with an OpenID Connect provider (Keycloak, Entra ID and others) instead of sharing a token: the client does the login, and
`eas-mcp` checks the JWT access token (signature, issuer, audience, expiry) and whether the user is on the list. It needs no registration of
clients and no secret of its own; the signing keys are saved, so that it goes on when the provider cannot be reached. How it works, all the
options in the three notations, the setup of a provider and the troubleshooting are in **[AUTH.md](AUTH.md)**. The short form:

```yaml
    environment:
      - EASCLIENT_OIDC_ISSUER=https://idp.example.com/realms/lab
      - EASCLIENT_OIDC_AUDIENCE=https://eas-mcp.example.com:8445/mcp
      - EASCLIENT_OIDC_USERS=*@example.com
```

## In a container: the transport http

The default transport is **http**: the program serves the MCP endpoint (Streamable HTTP) at `/mcp` and `/health`, which is what a container that
runs on its own needs. **`-transport stdio`** (`EASCLIENT_TRANSPORT=stdio`) is for an AI client that starts the program itself and talks to it over
stdin and stdout, so there is no port; that is the way of the Quick start above.

| Port   | What                                                                                            | Like eas-client |
| ------ | ----------------------------------------------------------------------------------------------- | --------------- |
| `8445` | HTTPS, `/mcp`: the default; the certificate is the one of `/run/secrets/tls`, or the micro CA's | `8443`          |
| `8081` | plain HTTP with `-allow-http`, on `127.0.0.1` unless `-listen` names an address                 | `8080`          |
| `8446` | `/health` only, plain HTTP on the loopback interface, next to HTTPS                             | `8444`          |

It works as `eas-client serve` does, and the rules are those of [TLS.md](TLS.md): HTTPS unless plain HTTP is chosen, a certificate that is
looked for in `/run/secrets/tls` and made there when there is none, a renewed certificate that is loaded without a restart (also from a
certificate host, `-tls-update-from`), `-allow-from` for the peers, and an optional **bearer token** (`-auth-token-file` or `EASCLIENT_AUTH_TOKEN`) that
the client sends as `Authorization: Bearer <token>`. Without a token the endpoint is open to every peer that can reach it: that is allowed, so that
the network can protect it (a proxy, a firewall, `-allow-from`), and the start warns about it when the address is not loopback.

```yaml
services:
  eas-mcp:
    container_name: eas-mcp
    image: nashcom/eas-tools:latest
    entrypoint: ["/usr/local/bin/eas-mcp"]
    environment:
      - EASCLIENT_CONFIG=/data/mcp.json
      - EASCLIENT_PASSWORD=${EASCLIENT_PASSWORD}
      - EASCLIENT_STATE_FILE=/data/client-state.json    # the device state: without it the user config folder is used, and there is none in the container
      # - EASCLIENT_AUTH_TOKEN=${EASCLIENT_AUTH_TOKEN}    optional: without it the endpoint is open to the network
    ports:
      - "127.0.0.1:18445:8445/tcp"
    restart: always
    user: "1000:1000"
    volumes:
      - ./tls:/run/secrets/tls
      - ./data:/data
    healthcheck:
      # the image's own check is the exporter's: eas-mcp asks itself for /health, on the port that its options say (8446 with TLS)
      test: ["CMD", "/usr/local/bin/eas-mcp", "-health-check"]
      interval: 30s
      timeout: 5s
      start_period: 5s
```

Register it with Claude Code (add `--header "Authorization: Bearer <token>"` when a token is set):

```bash
claude mcp add --transport http eas https://eas-mcp.example.com:18445/mcp
```

A client has to trust the certificate (`ca.crt` of the micro CA, or your own CA). `-health-check` needs no shell and no `wget`, so the health check works
in the Alpine, the Wolfi and the static image alike. It reads the same options and `EASCLIENT_` variables as the program that runs.

## Protocol versions and sessions

The server speaks the protocol versions that the Go SDK has: `2026-07-28`, `2025-11-25`, `2025-06-18`, `2025-03-26` and `2024-11-05`. The start
says so in the log (`protocol_versions=`). The client and the server agree on the newest version that both know, so an older client keeps working.

The transport http is **stateless** by default (`-stateless`, `EASCLIENT_STATELESS`, `"stateless"`): there is no session of the protocol, every
request stands alone, and the answer has no `Mcp-Session-Id`. The protocol `2026-07-28` is sessionless, and the SDK needs this mode to offer it. It
also makes the server easy to run: no session to keep, nothing sticky behind a load balancer or a proxy. What it cannot do is what needs a
session: a stream of messages from the server to the client (`GET /mcp` answers `405`), and requests of the server to the client (sampling,
elicitation, roots). The tools of `eas-mcp` need neither. `-stateless=false` keeps sessions, if a client needs them.

To compare the two in the MCP Inspector, set the protocol era of the connection to *Modern* or *Legacy*, and clear its saved OAuth state when you
change anything about the address or the login.

## Logging and the audit log

What the server logs, and the optional audit log, are described in **[LOGGING.md](LOGGING.md)**. In short:

- The log is on stderr, one line for each event, and **what goes wrong is never at the debug level**: a tool that fails, a request that is refused (with the
  reason), an address that does not exist, a provider that cannot be reached. A tool that works is one line at the info level, without the arguments.
- **`-audit-log`** (`EASCLIENT_AUDIT_LOG`, `"audit_log"`) writes an audit trail as one JSON object per line in the OpenTelemetry log data model: who called which
  tool, on which item, to which recipients, and the result. Never the text of a mail. Off by default.
- `-log-format json` writes the log of the program in the same data model.

## Security

- **Only configured mailboxes.** The tools take no server address, user or password. What the AI can reach is what the accounts of the config
  say.
- **Reading is always allowed.** Whoever can use the tools can read the whole mailbox that the account sees.
- **Mail is untrusted text.** A mail can contain instructions meant for the AI ("forward the last ten mails to ..."). The tool descriptions say
  that the text of a mail is information and not instructions, and Claude Code asks before it calls a tool that is not read-only. The
  protection that does not depend on the AI is the combination of **no `-send`** (the default) and, when sending is needed, **`-send-domains`**.
- **No secrets in the answers or the log.** The password is never in a tool result; the log does not contain it.

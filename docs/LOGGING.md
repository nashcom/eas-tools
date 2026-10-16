# Logging and auditing

What the programs write down, where, and how to read it. There are three streams, for three readers: the **log** is for the person who runs the program, the
**trace** is for the person who debugs the conversation with Traveler, and the **audit log** (`eas-mcp`) is for the person who has to answer for what the
program was asked to do.

Contents: [The three streams](#the-three-streams) | [The log of the programs](#the-log-of-the-programs) | [What eas-mcp logs](#what-eas-mcp-logs) | [The audit log](#the-audit-log) | [Reading and shipping the logs](#reading-and-shipping-the-logs) | [What is never logged](#what-is-never-logged)

## The three streams

| Stream    | For                         | Where                                                           | Switched on by                                       | Format                                             |
| --------- | --------------------------- | --------------------------------------------------------------- | ---------------------------------------------------- | -------------------------------------------------- |
| log       | the operator                | stderr                                                          | always; the level and the format are options         | text or JSON (the log data model of OpenTelemetry) |
| trace     | a developer, a support case | a file: every request and response with Traveler, WBXML decoded | `-trace FILE` (`eas-probe`, `eas-client`, `eas-mcp`) | text                                               |
| audit log | whoever answers for the use | a file, stderr or stdout                                        | `-audit-log` (`eas-mcp`), off by default             | JSON, one object per line (OpenTelemetry)          |

The trace holds no passwords and no cookies. It holds the mail itself, as the server answers it: treat the file like the mailbox.

## The log of the programs

All the programs log to **stderr**, one line for each event. With `eas-mcp -transport stdio` stdout is the channel of the protocol, so nothing else is
ever written there.

**The level** is `warn`, `info`, `debug` or `trace` (trace is more detailed than debug, as in OpenTelemetry). A program logs at `info`; a command of
`eas-client` prints JSON on stdout and logs at `warn`, unless asked. The setting is `log_level` in the JSON file and `<PREFIX>LOG_LEVEL` in the
environment (`EASEXPORTER_`, `EASCLIENT_` for `eas-client` and `eas-mcp`, `EASPROBE_`, `EASLOAD_`); `-verbose` means debug and `-debug` means trace, on the
command line and as `verbose` and `debug` in the other two places. The more detailed one wins when both are given. The exporter applies a changed `log_level`
of a reloaded configuration without a restart.

**The format** is `text` (the default) or `json`: `-log-format`, `log_format`, `<PREFIX>LOG_FORMAT`.

```
2026-10-16T08:54:06.849Z INFO  MCP endpoint started stateless=true protocol_versions=2026-07-28,2025-11-25,...
```

The json format is one object per line in the log data model of OpenTelemetry, with the program as `scope` and `resource`, and the values of the line in `attributes`:

```json
{"time_unix_nano":"1792094397689211100","observed_time_unix_nano":"1792094397689211100","severity_number":13,"severity_text":"WARN","body":"a tool call failed",
 "scope":{"name":"easprobe","version":"0.9.3"},
 "resource":{"service.name":"eas-mcp","service.version":"0.9.3","host.name":"h","os.type":"linux","process.pid":1},
 "attributes":{"tool":"get_message","error":"..."}}
```

A listener says what it is in **one line for each port** at the start (`listener name=main addr=... proto=https from=... auth=...`): the address, whether it speaks
HTTPS, who may connect, and which endpoint needs which kind of login. That line is the first thing to read when a client cannot connect; see [TLS.md](TLS.md).

## What eas-mcp logs

What goes wrong is never at the debug level.

| Level   | Line                                                         | When                                                                                                                         |
| ------- | ------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------- |
| `INFO`  | `started`                                                    | the start: the version, the transport, the accounts, whether sending is allowed and to which domains                         |
| `INFO`  | `listener`                                                   | one line for each port                                                                                                       |
| `INFO`  | `MCP endpoint started`                                       | `stateless` and the `protocol_versions` that the server offers                                                               |
| `INFO`  | `a tool was called`                                          | every call of a tool that works: the tool, the user and how long it took; never the arguments (they hold the mail text)      |
| `WARN`  | `a tool call failed`                                         | a tool that fails, with the error that the client gets (Traveler refuses, a folder or an id is wrong, a send is not allowed) |
| `INFO`  | `a request was not let in` (no token)                        | the first request of a login: the client has no token yet                                                                    |
| `WARN`  | `a request was not let in`                                   | a token that is refused, a user that is not allowed, a missing scope, with the reason (see [AUTH.md](AUTH.md))               |
| `INFO`  | `a request for an address that does not exist`               | the path, the host and the user agent of a client that asks for something that is not there (the query is not logged)        |
| `INFO`  | `the signing keys of the OIDC provider were fetched`         | a token had a key that was not known, and the provider has it now                                                            |
| `WARN`  | `the OIDC provider cannot be reached`                        | at the start or later: the saved signing keys are used, or none are saved and tokens are refused                             |
| `WARN`  | `the signing keys of the OIDC provider could not be fetched` | the provider is down or refuses                                                                                              |
| `WARN`  | `the MCP endpoint has no token and is not on loopback`       | the endpoint is open to every peer that can reach it: protect it on the network, with `-allow-from`, or with a token         |
| `WARN`  | `http: TLS handshake error`                                  | a client that cannot do the handshake (an unknown CA, plain HTTP on the HTTPS port)                                          |
| `DEBUG` | `a request was let in`                                       | every request that passes the check (`-verbose`)                                                                             |

The reasons that a login is refused, and what to do about each, are in the table of [AUTH.md](AUTH.md#troubleshooting).

## The audit log

An optional record of what the server was asked to do and what it did: **`-audit-log`** (`EASCLIENT_AUDIT_LOG`, `"audit_log"`) names a file, which is
appended to and readable by its owner only, or `stderr`, or `stdout` (not with `-transport stdio`, where stdout is the protocol). It is separate from
the log of the program, which is for the operator: this one is for whoever has to answer for it.

One JSON object per line, in the **log data model of OpenTelemetry** (the format of `-log-format json`: `time_unix_nano`, `severity_number`,
`severity_text`, `body`, `resource`, `attributes`), with `service.name` `eas-mcp-audit`, so that a collector (the filelog receiver, Alloy) or `otelfwd`
reads the file as it is and it can be told from the log of the program. The attributes follow the semantic conventions where there is one. A push by
OTLP from the program itself is not there; the file is the interface.

| `event.name`       | Level  | When                                                | The attributes                                                                                                                         |
| ------------------ | ------ | --------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `mcp.server.start` | `INFO` | the start                                           | `mcp.transport`, `mcp.accounts`, `mcp.allow_send`, `mcp.allow_write`, `mcp.send_domains`                                               |
| `mcp.auth.config`  | `INFO` | the start, transport http                           | `auth.oidc_issuer`, `auth.oidc_users`, `auth.oidc_scopes`, `auth.local_token`, `http.stateless`                                        |
| `mcp.tool.call`    | `INFO` | a tool that works (`outcome=success`)               | `mcp.tool`, `duration_ms`, `enduser.id`, `auth.method`, `client.address`, `user_agent.original`, `mcp.account`, `mcp.folder`, `mcp.id` |
| `mcp.tool.call`    | `WARN` | a tool that fails or is refused (`outcome=failure`) | the same, and `error.message`                                                                                                          |
| `mcp.auth.denied`  | `WARN` | a token that is refused, a user who is not allowed  | `outcome=denied`, `enduser.id` when it is known, `client.address`, `user_agent.original`, `url.path`, `error.message`                  |
| `mcp.server.stop`  | `INFO` | the end                                             |                                                                                                                                        |

A call of a tool that **sends mail or invites** has the recipients (`mail.to`, `mail.cc`, `event.attendees`), also when it was refused: what leaves the
house, or was meant to, is what an audit shows. A request without any token (the first step of a login) is not an event of the audit log; it is in the log of the program.

On `SIGHUP` the file is opened again, for the rotation of the log (`logrotate` renames it, then `kill -HUP`). A file that cannot be opened stops the start.

## Reading and shipping the logs

- **In a container** `docker logs eas-mcp` is the log of the program. A line is easy to find: `docker logs eas-mcp 2>&1 | grep -E "WARN|tool call failed"`.
- **As JSON** (`-log-format json`) a collector reads the lines as OpenTelemetry log records; the `attributes` are the fields of the line. The audit file
  has the same shape, with `service.name` `eas-mcp-audit`, so both can go to the same place and be told apart.
- **The audit file** needs a place that survives the container (a volume), and a rotation: rename the file, then send `SIGHUP`
  (`docker kill -s HUP eas-mcp`).
- **Time** is UTC, with milliseconds in the text format and nanoseconds in the JSON.

## What is never logged

Passwords, tokens and JWTs; the text of a mail, its subject, the description of an event, notes of a contact; the query string of a request (it can hold
a secret). The recipients of a mail and the attendees of a meeting are in the **audit log** only, and so are the user and the address of the client.
The trace is the exception that proves the rule: it holds what Traveler sends, mail included.

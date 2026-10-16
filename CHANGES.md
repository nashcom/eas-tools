# Changes

Newest version first. Every change is noted here with the change itself. All the programs of the toolbox have the same version (the constant
`Version` in `probe/profile.go`). `push-release.sh` refuses to tag a version that has no section here, and the release workflow makes the text of
the GitHub release from it.

## 0.9.3

**eas-mcp grows up, and is still experimental.** It now has everything that `eas-client` has, a login with OpenID Connect, and an audit log.

- **Calendar and contacts:** `list_events`, `get_event`, `get_event_ics`, `list_contacts`, `get_contact`, `get_contact_vcard`, `export_contacts`
  (read only), and `create_event` and `delete_event` (`-write`; a meeting with attendees needs `-send` too). Every command of `eas-client` has a
  tool now ([docs/MCP.md](docs/MCP.md) maps them); `no_sent_copy` is there for send, reply and forward.
- **Login with OpenID Connect:** the endpoint checks the JWT access tokens of your provider (signature, issuer, audience, expiry), and the user
  against `oidc_users` (`alice@example.com`, `*@example.com`, `*`). The signing keys are saved, so that it goes on when the provider is not
  reachable. The local token stays as the fallback. No client registration on this side. See [docs/AUTH.md](docs/AUTH.md), a new page about the
  authentication of all the programs.
- **The protocol 2026-07-28:** the transport http is stateless by default (`-stateless`); older clients work as before.
- **More in the log:** a tool that fails and a request that is refused say why, at the normal level; so does a request for an address that does not
  exist. **`-audit-log`** writes an optional audit trail, one JSON object per line in the OpenTelemetry log data model: who called which tool, on which
  item, to which recipients, and the result, never the text. See [docs/LOGGING.md](docs/LOGGING.md), a new page about logging.
- **`-health-check`** for `eas-client` and `eas-mcp`: a health check of a container that needs no shell, in all the images.
- **`-send-domains` now covers meetings:** the invitation and the cancellation of a meeting are mail to the attendees, and follow the list of domains
  in `eas-mcp`, `eas-client` and its REST API.
- **Every option of eas-mcp** can be given on the command line, as an `EASCLIENT_` variable and in the JSON file; a test checks that. An account of the
  file without a password uses `EASCLIENT_PASSWORD`.
- **The OpenAPI description of the REST API is complete:** every operation and every field has a text, and the operation ids are verbs (`listAccounts`, `getAccountInfo`, `listFolders`, `getMessageEml`, ...; they were `accounts`, `info`, `folders`, `emlMessage`). A test keeps it so.
- A new library, `go-jose` (the check of the signatures); the licenses are in `THIRD_PARTY.md`.

## 0.9.2

**eas-mcp, a first experimental addition: the mailboxes for AI clients.** `eas-mcp` is an [MCP](https://modelcontextprotocol.io) server on
the code of `eas-client`. It is **experimental**: the tools, the options and the defaults can still change, and sending mail has been tried
against the mock server only. See [docs/MCP.md](docs/MCP.md).

- **Tools:** `accounts`, `server_info`, `list_folders`, `list_messages` and `get_message` (read only), and `send_message`, which exists only
  with `-send`. Changing a mailbox, reply, forward, calendar and contacts are not tools yet.
- **Transports:** http is the default (the endpoint `/mcp` and `/health` on `8445`, HTTPS; `8081` plain HTTP with `-allow-http`; `8446` the
  health port), for a container. `-transport stdio` is for a client that starts the program. A bearer token is optional.
- **The same configuration as eas-client:** the options, the config file with `accounts` and the `EASCLIENT_` variables are shared (the new
  package `internal/clientcfg`). An account of the file that names no password uses `EASCLIENT_PASSWORD`.
- **`-send-domains`** (`send_domains`) limits the domains that mail is sent to, for send, reply and forward. It is in the client manager, so
  `eas-client` and its REST API have it too (`403 recipient_not_allowed`). Without it there is no limit.
- **Licenses:** the license texts of the libraries are in `third_party/licenses/` and in the images (`/usr/share/licenses/eas-tools`), the
  list is in [THIRD_PARTY.md](THIRD_PARTY.md); `scripts/licenses.sh` makes them.
- The images contain `eas-mcp`; `compile.sh` builds it.

## 0.9.1

**A renewed TLS certificate can be fetched from a certificate host.** The exporter and the client ask a host that keeps the certificates (the
Domino CertMgr, for one) for the certificate of their own name, and take it when it is better than the one they have.

- **Settings:** `tls_update_from` (`host:port` of the certificate host; off when empty), `tls_update_name` and `tls_update_interval_seconds`
  (default 3600) for the exporter; `-tls-update-from`, `-tls-update-name` and `-tls-update-interval` (default 1h) for the client.
- **The name** that is asked for (SNI) is `tls_update_name`, or else the host name of the machine; it is never taken from a certificate, and
  the host answers with the best match, a wildcard certificate included.
- **What is taken:** a certificate that is for the private key in use, valid now, for the name, and newer than the one in use. One that does
  not meet all four is not taken (another key is a warning). The certificate file is replaced atomically; the key file is never touched.
- **When:** at the start, then every interval, and at once on `POST /config/reload` of the exporter and on `SIGHUP`. The log says at the
  info level that the host was asked, also when there is nothing new.
- **A key and no certificate:** the certificate is fetched before anything else, and the start fails with the reason when the host has none
  that fits.
- The helpers (`FetchChain`, `Evaluate`, `AskName` and others) are in `internal/certreload/certmgr.go` and depend on the standard library
  only, so that the file can be reused. Described in [docs/TLS.md](docs/TLS.md#from-a-certificate-host).

## 0.9.0

**The first version of eas-tools, an ActiveSync toolbox for HCL Traveler.** Three deliverables and two supporting tools, all with this version.

- **Blackbox exporter** (`eas-blackbox-exporter`): Prometheus scrapes `/probe?target=NAME` and gets the metrics of an ActiveSync probe: login,
  Provision, FolderSync, Sync, a held Ping, a mail round trip, and the mail monitor for mail to an address outside. Configuration from a file,
  from environment variables or from an `https://` location that is checked on a timer, with reload; alert rules and scrape jobs in `deploy/`.
- **Performance test** (`eas-load` with the coordinator [nshtestherd](https://github.com/nashcom/nshtestherd), 0.9.2 or newer): simulated users and
  devices with the jobs probe, directory lookups, sync, push and mail; `scripts/roundtrip.sh` and `scripts/roundtrip.ps1` run the whole chain in one
  command and check it.
- **EAS client** (`eas-client`): mail (list, get, send, reply, forward, read, move, delete, `.eml`), calendar (list, get, `.ics`, create with
  invitations, delete) and contacts (list, get, vCard) from the command line and through a REST API that prints the same JSON, with OpenAPI and
  Swagger UI. Reading is always allowed; changing the mailbox (`-write`) and sending (`-send`) are two separate switches.
- **`eas-probe`**, one probe on the command line (the engine of the exporter), and **`eas-mock-server`**, a stand-in ActiveSync server for demos,
  development and CI.
- **Secure by default.** The exporter and the client serve HTTPS unless plain HTTP is chosen (`allow_http`), and plain HTTP is on the loopback
  interface unless another address is named. The certificate comes from `/run/secrets/tls` (`tls.crt`, `tls.key`, `ca.crt`), or the program
  makes one with a small CA of its own, which is for a try and is made again at every start; a certificate of your own is never touched and is
  picked up when it is renewed. An allow list of addresses and networks (`allow_from`) guards every endpoint. The metrics of the exporter are on a
  port of their own (9100). The log of the start has one line for each port.
- **Container images** for amd64 and arm64: `ghcr.io/nashcom/eas-tools` (Alpine), `:wolfi` and `:static`, all non-root, with every capability
  dropped.
- **One dash for the options** in all the tools: `-server`, `-password-file`. The double dash is still read where it was read before.

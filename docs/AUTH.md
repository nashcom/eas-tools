# Authentication and access control

Who may call the programs of eas-tools, and how they prove it. The programs that listen (the exporter, `eas-client serve` and `eas-mcp`) have the
same layers, from the outside in: the **network**, **TLS**, **authentication** (who is calling) and **authorization** (what that caller may do).
This page is about authentication; TLS has its own page, [TLS.md](TLS.md). How the programs log in to Traveler is in
[GUIDE.md](GUIDE.md#specifying-a-traveler-server).

Contents: [Overview](#overview) | [The layers](#the-layers) | [The local token](#the-local-token) | [OIDC for eas-mcp](#oidc-for-eas-mcp) | [The settings](#the-settings-of-the-authentication-of-eas-mcp) | [Setting up a provider](#setting-up-a-provider) | [Trying it](#trying-it) | [Troubleshooting](#troubleshooting) | [What it does not do](#what-it-does-not-do)

## Overview

| Program                 | Endpoint                                         | Who may call it                                                                        | Details                               |
| ----------------------- | ------------------------------------------------ | -------------------------------------------------------------------------------------- | ------------------------------------- |
| `eas-blackbox-exporter` | `/probe`, `/debug/state`, `/config/reload`       | the peers of `allow_from`; with `auth_token` also a token (Bearer)                     | [EXPORTER.md](EXPORTER.md)            |
| `eas-blackbox-exporter` | `/metrics`, `/health` (also on the metrics port) | the peers of `allow_from` (`allow_from_metrics` on the metrics port); no token         | [TLS.md](TLS.md#ports-and-plain-http) |
| `eas-client serve`      | the REST API                                     | the peers of `allow_from`; a token (Bearer), required unless the address is loopback   | [CLIENT.md](CLIENT.md)                |
| `eas-mcp`               | `/mcp`                                           | the peers of `allow_from`; **OIDC** (a JWT of your provider), the local token, or both | below                                 |
| all of them             | `/health` (the health port next to HTTPS)        | loopback, or the peers of `allow_from_health`; no token                                | [TLS.md](TLS.md#ports-and-plain-http) |

The token of the exporter and of the client is one secret that every caller shares. OIDC is the other kind: every caller has an identity of its own,
and the server checks it against a list of the users that are allowed.

## The layers

1. **The network.** `allow_from` (addresses, networks, ranges, `private`) is checked first, for every endpoint. Loopback is always allowed. A
   firewall or a reverse proxy in front is better still. Without a token and without OIDC this is the only protection of an endpoint.
2. **TLS.** HTTPS is the default; plain HTTP has to be chosen (`allow_http`) and is for a proxy that terminates TLS. A token and a JWT are
   secrets: over plain HTTP on a network they cross it in clear text. See [TLS.md](TLS.md).
3. **Authentication.** A token or a JWT in the header `Authorization: Bearer ...`. A token is never taken from the URL, which ends up in logs
   (the one exception is `?auth=` on `/probe` of the exporter, which exists for scrapers that cannot send a header).
4. **Authorization.** What the caller may do once it is known: for `eas-mcp` the list of the users (`oidc_users`), the scopes, and the switches
   `-send` and `-write` and the list of domains `-send-domains`; for `eas-client` the same two switches. Reading is always allowed.

## The local token

One shared secret. It is the simple way, and the way for scripts, the exporter and the client.

```bash
openssl rand -hex 32 > token ; chmod 600 token
```

| What                  | Command line                                                                        | Environment                 | JSON file           |
| --------------------- | ----------------------------------------------------------------------------------- | --------------------------- | ------------------- |
| a file with the token | `-auth-token-file`                                                                  | `EASCLIENT_AUTH_TOKEN_FILE` | `"auth_token_file"` |
| the token itself      | not an option: a secret on the command line is visible to every user of the machine | `EASCLIENT_AUTH_TOKEN`      | `"auth_token"`      |

(For the exporter the names are `auth_token`, `auth_token_file`, `auth_token_env` and the variables with the prefix `EASEXPORTER_`.)
The client sends `Authorization: Bearer <token>`. The token is compared in constant time. A change of the token needs a restart.

## OIDC for eas-mcp

With OIDC a user logs in at your OpenID Connect provider (Keycloak, Entra ID and others) and the MCP client sends the access token that the
provider issued. `eas-mcp` checks the token; it needs **no registration of clients and no secret of its own**.

```
MCP client                      eas-mcp                         OIDC provider
    |  POST /mcp (no token)         |                                  |
    |------------------------------>|                                  |
    |  401 WWW-Authenticate: Bearer resource_metadata="https://.../.well-known/oauth-protected-resource/mcp"
    |<------------------------------|                                  |
    |  GET the metadata (no token)  |                                  |
    |------------------------------>|                                  |
    |  {resource, authorization_servers: [issuer]}                     |
    |<------------------------------|                                  |
    |  discovery, registration, login of the user (browser), token     |
    |----------------------------------------------------------------->|
    |<-----------------------------------------------------------------|
    |  POST /mcp  Authorization: Bearer <JWT>                          |
    |------------------------------>|  keys of the provider (cached)   |
    |                               |--------------------------------->|
    |  the answer of the tool       |  checks the JWT and the user     |
    |<------------------------------|                                  |
```

The client finds the provider by the *protected resource metadata* of RFC 9728 that the `401` answer points to (the picture above).

What `eas-mcp` checks in a token:

- the **signature**, with the keys of the provider (asymmetric algorithms only: RS256/384/512, PS256/384/512, ES256/384/512, EdDSA; `none` and the
  shared-secret algorithms are refused);
- the **issuer** (`iss`) is the configured issuer, exactly;
- the **audience** (`aud`) holds the configured audience: the token has to be issued for *this* server, so that a token that another service
  accepts cannot be used here;
- the **time**: `exp` is required and checked, `nbf` when it is there, with 30 seconds of tolerance for the clocks;
- the **user**: the e-mail address of the token (or another claim) matches one of the patterns of `oidc_users`;
- the **scopes**, if `oidc_scopes` names some.

A client without a token, with a token that is wrong, expired or for another audience gets `401` (with the pointer to the metadata), and a
user that is not on the list gets `403`. The reason is in the log at the debug level; the token is never logged.

**Clients that look for the login server here.** Some clients (the MCP Inspector 2.10, for one) do not use the login server that the
metadata names. They ask the MCP server itself for /.well-known/oauth-authorization-server and /.well-known/openid-configuration. `eas-mcp` is not
an authorization server, but it answers these two addresses with the discovery document of your provider; every endpoint in it (authorization,
token, keys) is the provider's, so the client logs in there, and `eas-mcp` signs and issues nothing. What the `issuer` in the document is depends
on what the client checks, and the two checks that clients make contradict each other:

| `-oidc-compat-issuer` | The `issuer` in the document            | Fails for a client that checks                                                            |
| --------------------- | --------------------------------------- | ----------------------------------------------------------------------------------------- |
| `provider` (default)  | the issuer of the provider, as it is    | the issuer against the address that the document came from (RFC 8414)                     |
| `origin`              | the address of this server              | the `iss` of the answer of the provider against the issuer (RFC 9207), which Domino sends |
| `off`                 | no document: the addresses answer `404` | a client that has nowhere else to look                                                    |

The error `Issuer mismatch in authorization response (RFC 9207)` means that `origin` is on and the provider sends `iss`: use `provider`. A client
that follows `authorization_servers` of the metadata (Claude Code does) asks the provider itself and needs none of this: use `off` for it. A
server that has not fetched the document yet answers `503`. `/register`, `/authorize` and `/token` do not exist here and answer `404`; a provider
without dynamic registration (Domino) needs a client that is registered by hand.

**The signing keys.** They are found by OIDC discovery (`<issuer>/.well-known/openid-configuration`, then `jwks_uri`), kept in memory and saved to
`oidc-keys.json` (next to the state file, or `oidc_cache_file`). A key that is new (the provider has rotated) is fetched when a token
needs it, at most every 30 seconds. **Without a connection to the provider** the saved keys are used: at the start, and later. Without any
saved key the server starts anyway, says so, and refuses tokens until it can fetch them.

**The address of the server.** The metadata names the address of the endpoint, and the `401` answer points to it. Without `-public-url` that
is the address of the request: the host and port that the client used, so it is one that the client can reach, whether it is `127.0.0.1:18445`
or a name. Behind a reverse proxy the proxy has to send `X-Forwarded-Proto` and `X-Forwarded-Host`; or set `-public-url`. The address names nobody
but the caller: the answer goes back to the one who asked. The audience of the token is not taken from it: it is `-oidc-audience`.

**The local token as the fallback.** With `EASCLIENT_AUTH_TOKEN` set as well, a token that is not a JWT of the provider (or that the provider
refuses) is compared with the local token. OIDC is tried first.

## The settings of the authentication of eas-mcp

Every option can be given on the command line, as an environment variable and as a key of the JSON file; the order is command line,
environment, file, default. The environment variable is `EASCLIENT_` and the name in upper case, with `_` for `-`; the JSON key is the name
with `_` for `-`. A list is repeated or separated by commas on the command line and in the environment, and is a JSON array in the file.

| Command line          | Environment                         | JSON file              | What it says                                                                                                                                                                                                                                   |
| --------------------- | ----------------------------------- | ---------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-oidc-issuer`        | `EASCLIENT_OIDC_ISSUER`             | `"oidc_issuer"`        | the issuer URL (https), exactly as the provider writes it in its tokens; this switches OIDC on                                                                                                                                                 |
| `-oidc-audience`      | `EASCLIENT_OIDC_AUDIENCE`           | `"oidc_audience"`      | what the JWT has to be issued for (`aud`); **required** unless `-public-url` is set, which is its default                                                                                                                                      |
| `-public-url`         | `EASCLIENT_PUBLIC_URL`              | `"public_url"`         | optional: the address that the clients use, such as `https://eas-mcp.example.com:8445/mcp`; without it the address of each request is used (the host and port that the client used; `X-Forwarded-Host` and `X-Forwarded-Proto` behind a proxy) |
| `-oidc-users`         | `EASCLIENT_OIDC_USERS`              | `"oidc_users"`         | **required**: the users that are allowed: `alice@example.com`, `*@example.com`, `*` for everybody (case is ignored)                                                                                                                            |
| `-oidc-user-claim`    | `EASCLIENT_OIDC_USER_CLAIM`         | `"oidc_user_claim"`    | the claim that names the user; default: the first of `email`, `preferred_username`, `upn` and `sub` that is there                                                                                                                              |
| `-oidc-scopes`        | `EASCLIENT_OIDC_SCOPES`             | `"oidc_scopes"`        | scopes that the token has to have (`scope` or `scp`), and that the metadata offers; default: none                                                                                                                                              |
| `-oidc-cache-file`    | `EASCLIENT_OIDC_CACHE_FILE`         | `"oidc_cache_file"`    | where the signing keys are saved; default: `oidc-keys.json` next to the state file                                                                                                                                                             |
| `-oidc-compat-issuer` | `EASCLIENT_OIDC_COMPAT_ISSUER`      | `"oidc_compat_issuer"` | what the old login server addresses answer: `provider` (default), `origin` or `off`, see below                                                                                                                                                 |
| `-ca-file`            | `EASCLIENT_CA_FILE`                 | `"ca_file"`            | a CA that is trusted for the provider as well (a provider with a certificate of a private CA)                                                                                                                                                  |
| `-auth-token-file`    | `EASCLIENT_AUTH_TOKEN_FILE`         | `"auth_token_file"`    | the local token, from a file (first line)                                                                                                                                                                                                      |
| not an option         | `EASCLIENT_AUTH_TOKEN`              | `"auth_token"`         | the local token itself                                                                                                                                                                                                                         |
| `-allow-from`         | `EASCLIENT_ALLOW_FROM`              | `"allow_from"`         | the peers that may connect, for every endpoint                                                                                                                                                                                                 |
| `-send`, `-write`     | `EASCLIENT_SEND`, `EASCLIENT_WRITE` | `"send"`, `"write"`    | the switches for sending mail and for changing a mailbox: off unless given                                                                                                                                                                     |
| `-send-domains`       | `EASCLIENT_SEND_DOMAINS`            | `"send_domains"`       | the domains that mail may go to; default: no limit                                                                                                                                                                                             |

The same, in the three notations:

```bash
eas-mcp -config /data/mcp.json -oidc-issuer https://idp.example.com/realms/lab -oidc-audience https://eas-mcp.example.com:8445/mcp -oidc-users '*@example.com'
```

```yaml
    environment:
      - EASCLIENT_OIDC_ISSUER=https://idp.example.com/realms/lab
      - EASCLIENT_OIDC_AUDIENCE=https://eas-mcp.example.com:8445/mcp
      - EASCLIENT_OIDC_USERS=*@example.com,admin@nashcom.de
      - EASCLIENT_AUTH_TOKEN=${EASCLIENT_AUTH_TOKEN}      # optional: the fallback
```

```json
{
  "oidc_issuer": "https://idp.example.com/realms/lab",
  "oidc_audience": "https://eas-mcp.example.com:8445/mcp",
  "oidc_users": ["*@example.com", "admin@nashcom.de"],
  "oidc_scopes": ["mail.read"],
  "send": false,
  "accounts": [ { "name": "lab", "server": "traveler.example.com", "user": "alice@example.com" } ]
}
```

A test goes through every option of the program and checks that it works in all three notations, so a new option cannot be left out.
The `-oidc-...` options without `-oidc-issuer` stop the start with a message, and so does `-oidc-issuer` without users or an audience.

## Setting up a provider

The same few things are needed with every provider. The details of the screens are the provider's; what is below is what to look for, and it
has not been tried with every product.

1. **A client for the MCP clients.** The AI client (Claude Code, the MCP Inspector, ...) logs the user in and registers itself, with dynamic
   client registration if the provider offers it. Without that (Entra ID, for one) register a public client by hand, with the redirect URI
   that the AI client documents, and give its id to the AI client.
2. **The audience.** The access token has to carry `eas-mcp`'s audience in `aud`. With a provider that supports resource indicators (RFC 8707)
   the client asks for the resource, which is the public URL. With others, add an audience mapping to the client (Keycloak: an *Audience*
   mapper with the public URL as the custom audience), or use the name that the provider puts in `aud` as `-oidc-audience`
   (Entra ID: the application id or the application ID URI; check the version of the tokens, v2.0 is the one with the claims below).
3. **JWT access tokens.** The access token has to be a signed JWT. A provider that issues opaque tokens cannot be used: introspection is not
   supported.
4. **The user.** The token needs a claim that names the user, by default the e-mail address. If the provider does not put `email` into access
   tokens, add it with a mapper, or name another claim with `-oidc-user-claim`.
5. **The issuer.** Take `issuer` from `<issuer>/.well-known/openid-configuration` of the provider, character for character (some providers end
   it with `/`).

## Trying it

What a client sees, without a token:

```bash
curl -si -X POST https://eas-mcp.example.com:8445/mcp -d '{}' | grep -i -E '^HTTP|www-authenticate'
curl -s https://eas-mcp.example.com:8445/.well-known/oauth-protected-resource/mcp
```

The first answer is `401` with `resource_metadata=...`, the second names the provider in `authorization_servers`.

- **Claude Code:** `claude mcp add --transport http eas https://eas-mcp.example.com:8445/mcp`, then `/mcp` in Claude Code and the login of the
  server `eas`. A client that cannot do the login can send a local token: `--header "Authorization: Bearer <token>"`.
- **MCP Inspector:** transport *Streamable HTTP*, the URL of the endpoint, and either the OAuth login of the Inspector or the bearer token in the
  Authentication section. It runs on Node: `NODE_EXTRA_CA_CERTS=ca.crt` makes it trust the micro CA of the server.

## Troubleshooting

What is logged, and the audit log, are in [LOGGING.md](LOGGING.md).

Every request that is not let in is in the log with the reason (`a request was not let in`, with the status, the path, the host, the remote
address and the user agent). A request without any token is at the info level: it is the normal first step of the login. A token that is
refused, a user that is not allowed and a missing scope are warnings, and the reason says what is wrong (for a JWT: the issuer, the audience or the
expiry that the token has, for a token that is no JWT: that an opaque token cannot be checked). The token is never in the log. With `-verbose` the
requests that are let in are in the log too, with the user. A request for an address that `eas-mcp` does not have
(a client that looks for a login server here, a typo in the URL, a scanner) is in the log at the info level, with the path, the host that the
client asked for and its user agent, and without the query: it shows what a client really asks for.

| What you see                                                                                | What it means                                                                                                                                                                       |
| ------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `401`, and the client does not start a login                                                | the client does not support the MCP authorization, or it cannot reach the metadata; read the metadata URL of the answer                                                             |
| the client says that fetching the metadata failed                                           | `-public-url` is set to an address that the client cannot reach (a placeholder, a name that does not resolve): correct it, or remove it, so that the address of the request is used |
| `401` after the login, log: `the token is not for this server`                              | the audience: the `aud` of the token is not `-oidc-audience`; fix the audience mapping at the provider, or the option                                                               |
| `401`, log: `the token is from the issuer ...`                                              | the issuer differs, often by a trailing `/`                                                                                                                                         |
| `401`, log: `no signing key of the provider verifies the token`                             | the key is unknown and the provider cannot be reached, or the token comes from another provider                                                                                     |
| `401`, log: `the token expired` or `is not valid before`                                    | the clocks of the provider and of the server differ by more than 30 seconds                                                                                                         |
| `401`, log: `not a signed JWT`                                                              | an opaque token: configure the provider to issue JWT access tokens                                                                                                                  |
| `403`, log: `a user of the OIDC provider is not allowed: it matches none of the oidc_users` | the user (the claim, in lower case) matches no pattern of `-oidc-users`; the log names the user                                                                                     |
| the log warns that the provider cannot be reached                                           | the saved keys are used; with none saved, tokens are refused until it can be reached                                                                                                |
| `the token names no user`                                                                   | the claim of `-oidc-user-claim` (default `email`) is not in the access token                                                                                                        |

## What it does not do

- **One mailbox for every authorized user.** OIDC says who is calling; the mailbox is the one of the configured account. Everybody who is let in
  reads that mailbox. A user-to-account mapping (a mailbox for each user) is not there. Use `-oidc-users` accordingly.
- **No token exchange.** The token of the user is checked and not passed on: Traveler is reached with the credentials of the account.
- **No opaque tokens, no introspection**, and no revocation list: a token is good until it expires. Keep the lifetime at the provider short.
- **No registration of clients**, and no login page of its own: that is the business of the provider and of the AI client.
- **No per-tool authorization.** The scopes and the users apply to the whole endpoint; what the AI may do is set by `-send`, `-write` and
  `-send-domains`.

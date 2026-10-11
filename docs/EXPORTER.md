# eas-blackbox-exporter: Prometheus monitoring of Traveler

The first deliverable of eas-tools. [Back to the overview](../README.md).

`eas-blackbox-exporter` serves the same probe to Prometheus, in the style of
the Grafana blackbox exporter and DomProbe: `GET /probe?target=NAME` runs an
ActiveSync probe of one configured account and returns Prometheus metrics.
Targets and credentials live in a JSON config file (`deploy/exporter-local.example.json` to run the program directly, `deploy/exporter-container.example.json` for the container);
passwords come from a file, from the config file itself or from an environment variable, never from the URL or
a command line.

Contents: [Quick start](#quick-start) | [Settings as environment variables](#settings-as-environment-variables) | [Configuration from an https:// location](#configuration-from-an-https-location) | [curl examples](#curl-examples) | [Kinds of probe, and mail](#kinds-of-probe-and-mail) | [Mail monitor (round trip to an address outside)](#mail-monitor-round-trip-to-an-address-outside) | [Endpoints](#endpoints) | [Configuration file](#configuration-file) | [Prometheus and container](#prometheus-and-container)

## Quick start

```
cp deploy/exporter-local.example.json config.json      # edit targets, password files (for the container: deploy/exporter-container.example.json)
./eas-blackbox-exporter -config config.json
```

## Settings as environment variables

With one target (the usual container) no config file is needed. The name of a variable is the key of the JSON file in
upper case with the prefix `EASEXPORTER_`: `listen` is `EASEXPORTER_LISTEN`, `state_file` is `EASEXPORTER_STATE_FILE`, and the
fields of the target work the same way (`EASEXPORTER_SERVER`, `EASEXPORTER_USER`, `EASEXPORTER_PASSWORD_FILE`,
`EASEXPORTER_MAIL`, ...). `EASEXPORTER_TARGET` is the name of the target in `/probe?target=NAME` (default `default`).

```
docker run -d --env-file deploy/exporter.example.env -v eas-data:/data -v ./pw.txt:/run/secrets/pw:ro -p 9117:9117 nashcom/eas-tools:latest
```

A JSON file can be used at the same time: a variable wins over the file. A variable of the target changes the target of
the file with the name in `EASEXPORTER_TARGET`, or adds a new one with that name. Without `EASEXPORTER_TARGET`, the variables
belong to the only target of the file when it has exactly one, so `config.json` with the target and one variable
`EASEXPORTER_PASSWORD` is a complete setup; with several targets, name the one you mean. More than one target needs the file. `insecure` and
`ca_file` are global and every target inherits them. The password of the target works as in the other programs:
`EASEXPORTER_PASSWORD` (the password itself, the key `password` of the JSON file, the simplest way), `EASEXPORTER_PASSWORD_FILE`
(a separate file, for example a Docker secret) or `EASEXPORTER_PASSWORD_ENV` (the name of another variable,
`password_env`); a file wins over the password, and the password over the variable name. A variable with the prefix that
matches no setting is logged as a warning, which catches typos.
which catches typos.

## Configuration from an https:// location

The whole configuration can be fetched from an `https://` URL instead of a file: `-config https://config.example.com/eas-tools.json`
or `EASEXPORTER_CONFIG`. Only `https://` is accepted (the certificate is checked, a redirect to `http://` is refused, and
the document may not be bigger than 1 MiB). How to fetch it is set locally, never in the document:

| Variable                                    | Meaning                                                                                                  |
| ------------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| `EASEXPORTER_CONFIG_TOKEN_FILE`             | file with the bearer token (first line); read at every fetch, so the token can be rotated                |
| `EASEXPORTER_CONFIG_TOKEN`                  | the token itself                                                                                         |
| `EASEXPORTER_CONFIG_TOKEN_NAME`             | optional header name for the token: default `Authorization: Bearer <token>`, otherwise `<name>: <token>` |
| `EASEXPORTER_CONFIG_CA_FILE`                | PEM file with additional trusted CA certificates for this location                                       |
| `EASEXPORTER_CONFIG_CHECK_INTERVAL_SECONDS` | how often to check for a change (default 60; 0 = never)                                                  |
| `EASEXPORTER_CONFIG_CACHE`                  | where the last good document is kept (default `/data/config-cache.json`, mode 0600)                      |

**Reload.** The same check works for a configuration file. The source is asked first whether it changed: the time and size of
a file, the `ETag` and `Last-Modified` of a URL (the server may answer `304 Not Modified`). What is read is compared by a hash
with the last accepted document (only the hash is kept): the same bytes need no further work. Different bytes are parsed and
compared with the settings in use, so a document that only looks different (spaces, key order) changes nothing and is not
counted as a reload. A new valid document replaces the targets and the limits while the exporter runs; a target that did not
change keeps its state. `listen`, `state_file` and `autosave_seconds` apply at the next start (the exporter logs it).
**When something fails** (the location is down, the token is refused, the document is invalid) the exporter keeps the last
good configuration, logs a warning and counts it in `eas_probe_config_reload_errors_total` (a counter, it only goes up: use
`increase()` for recent errors). The gauge **`eas_probe_config_status`** is the current state for alerting: `0` means the
last check worked, `1` means it failed and the exporter runs on the last good configuration (or on the saved copy after a
start with the location down). It returns to `0` at the next check that works, and `/debug/state` shows the reason and since
when. A restart while the location
is down starts from the saved copy. `eas_probe_config_last_check_timestamp_seconds` is the time of the last successful
check, `eas_probe_config_reloads_total` counts applied changes and `eas_probe_config_loaded_timestamp_seconds` says when the
configuration in use was loaded; `deploy/eas_probe_rules.yml` has alerts for a failing reload and a stale check.

**Token and its name.** Both tokens, the one that protects `/probe` and the one for fetching the configuration, are a value and
an optional name. The value is the token itself, or a file with it, or the name of a variable. The name is the HTTP header that
carries it. By default that is `Authorization: Bearer <value>`. When `Authorization` is already used for something else (a
Domino server or a proxy with its own login), give the token another name, for example `X-Auth-Token`: the header then holds
just the value, `X-Auth-Token: <value>`, and `Authorization` is left alone. For `/probe` the keys are `auth_token` and
`auth_token_name` (variables `EASEXPORTER_AUTH_TOKEN`, `EASEXPORTER_AUTH_TOKEN_NAME`), and `?auth=<value>` always works; for the
configuration they are `EASEXPORTER_CONFIG_TOKEN` and `EASEXPORTER_CONFIG_TOKEN_NAME`. A name that is used for other things
(`Cookie`, `Host`, `Content-Type`, ...) is refused.

**The log** says where the configuration comes from and whether that worked: `loading the configuration source=…`, then
`configuration loaded source=… from=remote|file|environment targets=N check_every=60s`, or a warning `…from="saved copy"…`
when the source was not usable, or an error `the configuration could not be loaded` with the reason. While running, a change
is logged as `configuration reloaded`, a failure as a warning, the first successful check after failures as `the
configuration source works again`, and every unchanged check at trace level (`-debug`). A URL is logged without its
query string.

**A file in a container.** When `config.json` is mounted as a single file (`./config.json:/etc/eas-tools/config.json:ro`) and an
editor replaces it with a new file instead of writing into it, the container keeps seeing the old one: Docker bound the old
file. Mount the folder instead, or edit the file in place, if the exporter should pick up the change.

**Mix and match.** The local environment variables apply on top of the document, so the remote file can hold the targets
and the local side can add what must not travel: for example `EASEXPORTER_TARGET=traveler-lab` with
`EASEXPORTER_PASSWORD_FILE=/run/secrets/pw` gives the password of that target, and a target that names `password_env` or
`password_file` reads it locally. Prefer these over a `password` in the remote document, because the document travels over the
network and the cache holds a copy.

## curl examples

```
# Probe the target "traveler-lab" (login, Provision check, FolderSync, Inbox Sync)
curl 'http://localhost:9116/probe?target=traveler-lab'

# With the bearer token (when auth_token_file / auth_token_env is configured)
curl -H 'Authorization: Bearer YOUR_TOKEN' 'http://localhost:9116/probe?target=traveler-lab'

# Also test a held Ping: keep a Ping open for 60 seconds (give curl a long enough timeout)
curl --max-time 120 -H 'Authorization: Bearer YOUR_TOKEN' \
     'http://localhost:9116/probe?target=traveler-lab&ping=60'

# Same with the token as URL parameter (ends up in logs, prefer the header)
curl 'http://localhost:9116/probe?target=traveler-lab&auth=YOUR_TOKEN'

# Only the key numbers
curl -s 'http://localhost:9116/probe?target=traveler-lab' | grep -E '^eas_probe_(success|step_duration|failure)'

# The exporter's own counters and a liveness check
curl http://localhost:9116/metrics
curl -i http://localhost:9116/health
```

After a refused login (HTTP 401/403) the target is left alone for
`auth_failure_backoff_seconds` (default 600); the answer keeps reporting the
failure with `eas_probe_auth_backoff 1` and `eas_probe_next_probe_seconds`. This
protects the account: a wrong password retried every scrape can trigger a
lockout or a ban rule on the server, which then hides the real problem.

A result is cached for `min_interval_seconds` (default 60) per target, so
repeated curls or a short scrape interval do not create extra logins on the
server. `eas_probe_cache_age_seconds` shows how old the answer is and
`eas_probe_timestamp_seconds` when the probe really ran.

## Kinds of probe, and mail

`/probe?target=NAME` is the plain probe. `&ping=60` adds a held Ping, `&mail=1` adds the mail round trip
(send a mail to the account itself, wait for it to come back, delete it). **Every kind has its own cached
result**, so a plain scrape never stands in for a ping or mail scrape, and the other way round. `mail=1` is
**opt-in per target** (`"mail": true` in the target; the login has to be an e-mail address) and creates a
mail on every real probe: give it its own Prometheus job with a long interval (see `deploy/prometheus.yml`).

The mail probe measures the round trip: the mail is sent, then the Inbox is synced every second until exactly this mail
shows up, and the mail is deleted again. `eas_probe_mail_roundtrip_seconds` is the number to watch (it has up to one
second of polling granularity). Push is not tested here: Traveler allows one open request per device, so the
`SendMail` of the same device aborts a held Ping and a push check with one device proves nothing. The separate
`&ping=60` probe shows that the server holds a Ping for the whole heartbeat.

## Mail monitor (round trip to an address outside)

`mail_monitor` in the configuration adds a probe that sends a mail from the ActiveSync account of a target to an
internet address and measures how long the **answer** takes to show up in the Inbox of the account. Something at the
other end has to answer: an agent that replies to mail with the tag `[nshmail-probe]` from allowed senders (the address
of the account) and keeps the subject. Example: `deploy/exporter-mail-monitor.example.json`.

The subject is `[nshmail-probe] probe-id=NAME-MS probe-posted=YYYYMMDDHHMMSS`: the tag, the id (the target name and the
time of sending in milliseconds, so an answer still counts after a restart) and the time of sending in UTC. The older tag
`[nshmailmon]` is accepted in answers.

`targets` and `mail_monitor` are lists: every entry has a `name`, which has to be unique.

| Key of an entry in `mail_monitor` | Default   | Meaning                                                                                    |
| --------------------------------- | --------- | ------------------------------------------------------------------------------------------ |
| `name`                            |           | the name of the target, the label `mail_monitor="NAME"` of the metrics                     |
| `to`                              |           | the internet address that the mail is sent to                                              |
| `account`                         | see below | name of a target whose account is used; needs `server` and an e-mail address as login      |
| `interval_seconds`                | 600       | time between two mails (at least 60); keep it long, the mails go through your mail routing |
| `timeout_seconds`                 | 300       | an answer later than this counts as lost                                                   |

The **account** that sends the mail and receives the answer is, in this order: the one the entry carries itself, the
target that `account` names, or the only target when there is exactly one. So with one target and one entry nothing has
to be said, and two entries can share an account or use different ones. An entry with an account of its own has the
fields `server` (required), `user` or `email` (an e-mail address), `password`, `password_file` or `password_env`, and
optionally `profile`, `provision`, `insecure`, `ca_file`, `headers`; it is not a target of `/probe`. `account` together
with these fields is an error. Entries that share a target share its session; an account of its own has its own session
and its own device. When the settings of an account change in a reload (a new password, another server), its session is
opened again.

`mail_monitor_poll_seconds` (default 2) is the time between two looks at the Inbox while a mail waits for its answer.

All targets run in **one** thread of the exporter and report in the normal `/metrics`; no scrape of `/probe` is
involved. The probe has a **device of its own** on the server, so it does not share sync keys with `/probe`. The single
round-trip times are kept for 60 minutes and the averages are calculated from them, the mails that were not answered for
300 minutes. Mails and answers are deleted again (the answer from the Inbox, the copy from Sent Items); other mail is
never touched. The probe is off unless `mail_monitor` is configured, and it creates mail: use a mailbox that expects it.

| Metric (`eas_probe_mail_monitor_...`)                                  | Meaning                                                                                                 |
| ---------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| `rtt_seconds{window="5m"}` (15m, 30m, 60m)                             | average round-trip time of the answers in that period (absent without answer)                           |
| `rtt_max_seconds{window}`, `rtt_samples{window}`                       | the longest one, and how many answers the average is made of                                            |
| `last_rtt_seconds`, `last_answer_timestamp_seconds`                    | the last answer                                                                                         |
| `lost_in_window{window="300m"}`, `delayed_in_window`, `sent_in_window` | mails lost now (no answer yet), mails that were lost and answered later, and mails sent, in 300 minutes |
| `sent_total`, `received_total`, `lost_total`                           | counters                                                                                                |
| `late_total`, `last_delayed_rtt_seconds`                               | answers after the timeout (the mail stays in `lost_total`), and the round trip of the last delayed mail |
| `send_errors_total`, `pending`, `up`                                   | sending failed, mails waiting for an answer, 1 when the account can be used                             |
| `cleanup_failed_total`                                                 | mails of the probe that could not be deleted                                                            |

A mail that was lost and is answered later is **delayed**: it counts once as lost (`lost_total`, which only goes up) and once as late
(`late_total`), and in the windows it moves from `lost_in_window` to `delayed_in_window` for as long as its record is kept
(300 minutes). A delayed mail is not part of the averages, which stay those of the mails that were answered in time.

A mail is lost only when the Inbox was read after its time was over; while the account cannot be read (`up` 0) nothing
is counted as lost. A refused login pauses the account for `auth_failure_backoff_seconds`.

A reload of the configuration applies changes of the entries at once and logs them: `mail monitor: entry added`,
`entry removed`, or `entry changed` with the old and the new value (`to`, `account`, `interval`, `timeout`), and a
changed account of an entry opens its session again. A reload that changes nothing logs nothing.

## Endpoints

| Endpoint                                                  | Meaning                                                                                       |
| --------------------------------------------------------- | --------------------------------------------------------------------------------------------- |
| `/probe?target=NAME[&ping=SECONDS][&mail=1][&auth=TOKEN]` | run (or return the cached) probe of a configured target; every kind has its own cached result |
| `/metrics`                                                | exporter counters: probes, errors, cache hits, rejected, devices, uptime                      |
| `/health`                                                 | always `200 OK`                                                                               |
| `/debug/state`                                            | the values in memory as JSON (see below); needs the token when one is set                     |
| `POST /config/reload`                                     | read the configuration source now and apply it when its settings changed; no body             |

**`/debug/state`** shows what the exporter holds in memory right now, as JSON, for a look at a running container. It is
not part of `/metrics`, so Prometheus never scrapes it. It is always on and follows the rule of `/probe`: with an
`auth_token` configured the request needs it (a wrong or missing token gets `401`), without one it is open. Set a token if
the port can be reached by others. It never shows a password or a token.

```
curl -H 'Authorization: Bearer TOKEN' http://localhost:9116/debug/state
```

- `exporter`: version, start, uptime, the targets, the devices in the state store, the counters, the load of the probe
  limit, when the configuration was loaded and last checked.
- `targets`: for each target whether a probe runs, the cached results (kind, age, size) and the end of an authentication backoff.
- `mail_monitor.targets`: for each entry the counters, the pending mails, the next mail, the four averages and, behind them,
  the **single values** of the last hour (`samples`: the time sent and the time received, each as a readable time (`sent_time`, `received_time`) and as Unix nanoseconds (`sent_unix_nano`, `received_unix_nano`), and `rtt_seconds`) and `problems`: the mails of the last 300 minutes
  that were not answered in time, each `lost` or `delayed` (lost, and answered after all, with its round trip).
- `mail_monitor.accounts`: for each account whether the session is open, the time of the next try and the last error, the
  mails that wait for an answer (tag, age) and the copies in Sent Items that are still to be deleted.
- `log`: the log level and format in use.

**`POST /config/reload`** checks the configuration source now, so that a change does not wait for the next check
(`EASEXPORTER_CONFIG_CHECK_INTERVAL_SECONDS`). It is a `POST` because it does something, and it has no body:

```
curl -X POST http://localhost:9116/config/reload
```

It runs the same steps as the timer (`304 Not Modified`, the hash, the comparison of the settings) and answers with what it
found: `"result": "applied"` (with the targets and mail monitor entries now in use), `"unchanged"` (with the reason) or
`"failed"` (with the error; the running configuration is kept, status `500`). It follows the rule of `/probe`: with an
`auth_token` configured it needs it, without one it is open. A `GET` answers `405`. The source is only the one that is
configured, nothing can be sent with the request. Two requests within 5 seconds: the second gets `429`. A configuration
from environment variables alone has no source: `409`. The check runs in the same place as the timed one, so the two never
overlap, and it also works with an interval of `0`, which means "only on request". The same check runs when the process
gets **`SIGHUP`** (`docker kill -s HUP eas-blackbox-exporter`); the result is in the log.

Responses: `200` with metrics (also when the probe itself failed:
`eas_probe_success 0` plus `eas_probe_failure{step,class}`), `400` bad
`ping`, `401` bad token, `404` unknown target, `503` too many probes running
or timed out waiting for the running probe of the same target.

The scrape timeout of Prometheus (`X-Prometheus-Scrape-Timeout-Seconds`) is
honored. A `ping=N` scrape needs a scrape timeout longer than N.

## Configuration file

| Key                                                 | Default                                   | Meaning                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| --------------------------------------------------- | ----------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `allow_from`                                        | none                                      | only these peers may connect, every endpoint (`/health`, `/metrics`, ...) included: a list of addresses, networks (`192.168.0.0/16`, `2001:db8::/32`), ranges (`10.0.0.5-10.0.0.20`) and the name `private` (RFC 1918 and `fc00::/7`); `EASEXPORTER_ALLOW_FROM` takes them separated by commas. The loopback interface is always allowed (the health check of the container). The address is the one of the connection: behind a proxy it is the proxy. A reload applies a change; refused requests are counted in `eas_probe_denied_requests_total`                                                                                                                                                                                                                                                                                                                                                                   |
| `tls_cert` / `tls_key`                              | `/run/secrets/tls/tls.crt`, `.../tls.key` | the certificate (the leaf first, then the chain) and its key. **TLS is the default**: when nothing is named, `allow_http` is not set and `listen` is not a loopback address, the files are looked for in `/run/secrets/tls` (the names of a Kubernetes TLS secret, with `ca.crt`), and **made there when they are not** (a small CA of its own, for a try; a certificate that it made is made again at every start, the CA stays; a certificate of yours is never touched). Both or none. The files are looked at every `tls_check_interval_seconds` and on `SIGHUP`: a renewed pair is loaded without a restart (a pair that does not fit, or is expired, is not taken and the one in use stays). A change of the two names needs a restart. `/metrics` has `eas_probe_tls_certificate_not_after_timestamp_seconds`, `eas_probe_tls_reloads_total` and `eas_probe_tls_reload_errors_total`. See [docs/TLS.md](TLS.md) |
| `tls_generate` / `tls_names`                        | off                                       | make the CA and the certificate at files that you name with `tls_cert` and `tls_key` (the default place needs no switch). `tls_names`: DNS names and IP addresses of the certificate, added to localhost, 127.0.0.1 and ::1; they replace the host name, which is in when there are none (in a container it is an id). A change takes effect at the next start. ECDSA P-256, valid `tls_valid_days` (365 by default, 1 to 3650); the CA (ten years) is kept next to it as `ca.crt` and `ca.key`. Files that are there are never overwritten                                                                                                                                                                                                                                                                                                                                                                            |
| `tls_check_interval_seconds`                        | 60                                        | how often the certificate and key files are looked at                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `listen_metrics` / `allow_from_metrics`             | `:9100`, open                             | the metrics port: a plain HTTP listener of its own with `/metrics` and `/health` only, always on, with TLS or without. A scrape needs no encryption and carries no secret, so it is open (the interfaces of `listen`) and `allow_from_metrics`, a list like `allow_from`, restricts it; `/probe` with its token, `/debug/state` and `/config/reload` are on `listen` only, which serves `/metrics` as well. The container health check uses this port. See [docs/TLS.md](TLS.md)                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `allow_http`                                        | off                                       | serve plain HTTP on purpose, for a proxy that terminates TLS (name it in `allow_from`): it turns the default certificate off. An error together with `tls_cert`. A loopback `listen` (a local NGINX in front) needs no switch and gets no certificate; on Kubernetes keep TLS behind the ingress                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `listen`                                            | `:9117` with TLS, `:9116` without         | main listen address: HTTPS by default (9117); plain HTTP (9116) with `allow_http` or on a loopback address. The ports are above 1024 on purpose, for the non-root container: change them on the outside (`-p 443:9117`); see [docs/TLS.md](TLS.md)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `auth_token` / `auth_token_file` / `auth_token_env` | none                                      | token for `/probe`: the value, a file with it, or the name of a variable (this order); `auth_token_name` is the optional header name, default `Authorization: Bearer <value>`, for another name just `<name>: <value>`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `state_file`                                        | none                                      | device state (policy and sync keys); keep it on a volume                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `autosave_seconds`                                  | 30                                        | state snapshot interval                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `min_interval_seconds`                              | 60                                        | minimum time between real probes of one target                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `auth_failure_backoff_seconds`                      | 600                                       | pause after a refused login (401/403); probing a wrong password every minute would get the account or client address blocked                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `max_concurrent`                                    | 10                                        | probes running at the same time                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `max_ping_seconds`                                  | 120                                       | upper limit of `ping=`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `default_timeout_seconds`                           | 30                                        | probe timeout without a Prometheus header                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `insecure`, `ca_file`                               | off                                       | TLS options, can be overridden per target                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `targets`                                           |                                           | a list; every entry has a `name` (the NAME of `/probe?target=NAME`), `server` (or `email`), `user`, `password`, `password_file` or `password_env`, `profile`, `device_id`, `provision`, `directory`, `mail`, `headers`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |

## Prometheus and container

`deploy/prometheus.yml` has the scrape jobs (regular probe, a slower
`ping=60` job and the exporter's own metrics), `deploy/eas_probe_rules.yml`
example alerts. `container/` has three Dockerfiles (Alpine, Wolfi, static) that build a non-root image
with a built-in health check (`eas-blackbox-exporter -health-check`); `build.sh` builds it (see `docs/build.md`).
The Alpine image is 33 MB and has been run against the lab. `docker compose up -d` runs the
exporter standalone from `docker-compose.yml` (targets in `config.json`, copied from `deploy/exporter-container.example.json`; the variant with only an env file is `deploy/exporter-env.compose.example.yml`).

```
./build.sh
```

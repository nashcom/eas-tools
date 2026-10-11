# How the tools work together

This guide explains the parts of the toolbox, how to point them at a Traveler
server (or at the mock server), and two complete walk-throughs: first **without** a
Traveler server, then **with** one. For the options of each tool see the
[README](../README.md) and the document of each tool: [exporter](EXPORTER.md), [performance test](LOADTEST.md), [client](CLIENT.md), [probe](PROBE.md), [mock server](MOCKSERVER.md).

Contents: [The parts](#the-parts) | [Specifying a Traveler server](#specifying-a-traveler-server) |
[Working with the mock server](#working-with-the-mock-server) |
[End to end A: without Traveler](#end-to-end-a-without-traveler) |
[End to end B: with Traveler](#end-to-end-b-with-traveler) |
[Metrics cheat sheet](#metrics-cheat-sheet) | [Troubleshooting](#troubleshooting)

## The parts

```
                      one account, one probe              many accounts, many devices
                      ──────────────────────              ───────────────────────────

  Prometheus ──scrape──▶ eas-blackbox-exporter                 nshtestherd (coordinator)
  Grafana                │   /probe?target=NAME              │  accounts, commands, status
                         │                                   │
                         │                                   ▼   register / status / command
  eas-probe (CLI) ────────┤                               eas-load (worker, N users x M devices)
   one-shot probe        │                                   │
                         ▼                                   ▼
                  ┌───────────────────────────────────────────────┐
                  │   probe package: one Session per device       │
                  │   options, provision, settings, folders, sync,│
                  │   directory (GAL), ping                       │
                  └───────────────────────┬───────────────────────┘
                                          │ ActiveSync over HTTP(S)
                                          ▼
                       Traveler (behind nginx, ...)   or   eas-mock-server (mock)
```

| Tool                    | What it is                                                                                  | Talks to                          |
| ----------------------- | ------------------------------------------------------------------------------------------- | --------------------------------- |
| `eas-probe`             | One-shot probe of one account; text, JSON or Prometheus output                              | the ActiveSync server             |
| `eas-blackbox-exporter` | Prometheus blackbox-style exporter: `/probe?target=NAME`, cached, with backoff              | the ActiveSync server             |
| `eas-load`              | Load worker: N simulated users with M devices each, driven by the coordinator or standalone | coordinator and ActiveSync server |
| `nshtestherd`           | The real coordinator (separate project): hands out accounts, sends commands                 | workers (eas-load)                |
| `eas-mock-server`       | Mock ActiveSync server for demos and tests (not a mail server)                              | tools                             |

The probe tools (`eas-probe`, `eas-blackbox-exporter`) answer "is it working, and how
fast?" for one account. `eas-load` answers "what happens with many users and
devices?". They share the same ActiveSync code, so a number measured by one
means the same as in the other.

What a device does, in order: `options` (versions and commands the server
offers) → `provision` (register the device, once) → `settings` (device
information like a phone sends) → `foldersync` → `sync` of the Inbox →
`directory` (look for the own user in the address book) → `ping` (held
connection). State (policy key, sync keys, folders) is kept per device in a JSON
file, so later runs are incremental and do not register again.

## Specifying a Traveler server

A Traveler server is an HTTP(S) endpoint, normally
`https://<host>/Microsoft-Server-ActiveSync`, possibly behind a reverse proxy
(nginx in your lab). Every tool takes the same notion of server:

| You pass                          | The tools use                                                   |
| --------------------------------- | --------------------------------------------------------------- |
| `traveler.example.com`            | `https://traveler.example.com/Microsoft-Server-ActiveSync`      |
| `traveler.example.com:8443`       | `https://traveler.example.com:8443/Microsoft-Server-ActiveSync` |
| `https://traveler.example.com/`   | the same default path is added                                  |
| `https://proxy.example.com/eas/x` | used as given (a path is kept)                                  |
| `http://127.0.0.1:8099`           | plain HTTP: only for the mock server or a closed lab            |

Where it goes in each tool:

| Tool                    | Server                                  | Login                                             | Password                                                      |
| ----------------------- | --------------------------------------- | ------------------------------------------------- | ------------------------------------------------------------- |
| `eas-probe`             | `-server traveler.example.com`          | `-user alice@example.com`                         | `-password-file F`, `-password-stdin`, or `EASPROBE_PASSWORD` |
| `eas-blackbox-exporter` | `"server"` of a target in `config.json` | `"user"` of the target                            | `"password_file"` or `"password_env"`                         |
| `eas-load`              | `-server traveler.example.com`          | `-login internetaddress` (default) or `shortname` | the password the coordinator hands out                        |

**Login name.** Use what the device would log in with. In your lab that is the
e-mail address (`nsh@lab.dnug.eu`). Domino may accept other forms (short name,
full name) depending on the Internet authentication setting of the server; if
one form gets a `401`, try another. `eas-load` takes the login from the account
that the coordinator hands out: the internet address by default.

**Autodiscover.** Without `-server`, `eas-probe -email alice@example.com` tries
the Autodiscover endpoints of the domain. Traveler often does not answer them:
give `-server` when you know it.

**TLS.** Certificates are verified. A private CA: `-ca-file myca.pem` (exporter:
`"ca_file"`). For a quick test only: `-insecure` (exporter: `"insecure": true`).

**Is the address right?** Before any tool, check with curl that the endpoint
answers and shows the server headers (type the password at the prompt):

```
curl -i -X OPTIONS -u 'alice@example.com' https://traveler.example.com/Microsoft-Server-ActiveSync
```

A good answer is `200` with `MS-ASProtocolVersions` and `MS-ASProtocolCommands`
headers (Traveler also sends `X-IBM-TRAVELER-...`). `401` means the login was
refused, `404` a wrong path, a TLS error the wrong certificate setup.

**Behind a proxy.** The proxy must pass `Authorization` through (nginx does by
default) and allow long requests: a Ping is held for the heartbeat, so
`proxy_read_timeout` must be longer than that (a real phone uses up to several
minutes; nginx's default is 60 s):

```
proxy_http_version 1.1;
proxy_read_timeout 900s;
proxy_send_timeout 900s;
proxy_buffering off;
proxy_request_buffering off;
client_max_body_size 50m;
```

Test it: `eas-probe ... -ping 120`. A `held_seconds` near 120 (Traveler answers a
few seconds early) means the proxy lets long requests through.

## Working with the mock server

`eas-mock-server` is a mock of a Traveler server, so that everything can be tried
and tested without one. It speaks real ActiveSync (WBXML) but has no mail
store: it answers with a tiny made-up mailbox.

```
./eas-mock-server -listen 127.0.0.1:8099 -mail-every 10s -latency 50ms
```

| Option        | Default          | What it does                                                                   |
| ------------- | ---------------- | ------------------------------------------------------------------------------ |
| `-listen`     | `127.0.0.1:8099` | address (plain HTTP)                                                           |
| `-password`   | `secret`         | the password of every account; any user name is accepted                       |
| `-mail-every` | off              | a new mail arrives for every device each interval (default: one mail per sync) |
| `-latency`    | 0                | delay added to every answer, e.g. `50ms`                                       |
| `-error-rate` | 0                | share of requests answered with HTTP 503 (0..1), to see error handling         |
| `-hold-ping`  | on               | hold a Ping for its heartbeat; a new mail wakes it early                       |
| `-drop-mail`  | off              | accept SendMail but never deliver the mail, to see a lost mail                 |

What it answers: `OPTIONS` (EAS 2.5 to 14.1 with Traveler-like headers and a
session cookie), `Provision` (two phases), `Settings`, `FolderSync` (Inbox and
Calendar), `Sync` (new mail, paged five at a time), `Search` (the address book:
every user that has logged in), `Ping`. It also answers `449` after a policy
loss when asked to (tests only).

How to use it: pass `http://127.0.0.1:8099` as the server, any user name, the
the mock server's password. A user that has not logged in yet is not in its address book.

What it cannot tell you: anything about the real Traveler, Domino or your
network. It is there to check that the tools work and that their numbers are
right (known latency in, known latency out), and to try Prometheus and Grafana
with live data. The coordinator for the load part is the real nshtestherd, as a container:

```
docker run --rm -p 8788:8788 nshtestherd:latest -bind 0.0.0.0 -generate 20 -password secret
```

It hands out `load000001@example.com` ... with that password, takes status
reports and accepts commands for all clients or for a single one (see the nshtestherd documentation).

## End to end A: without Traveler

Everything on one machine, five terminals. Linux or Git Bash syntax; on Windows
PowerShell use `.\eas-probe.exe` and `$env:NAME='value'`.

**In a hurry:** `scripts\roundtrip.ps1` (Windows) or `scripts/roundtrip.sh` (Linux) does all of this
in one command and checks the numbers (see [LOADTEST.md](LOADTEST.md)). The steps below show what it does.

**1. The mock server and the coordinator** (terminals 1 and 2)

```
./eas-mock-server  -listen 127.0.0.1:8099 -mail-every 10s -latency 30ms
docker run --rm -p 8788:8788 nshtestherd:latest -bind 0.0.0.0 -generate 20 -password secret
```

**2. One probe** (terminal 3). The mock server accepts any user with its password:

```
export EASPROBE_PASSWORD=secret
./eas-probe -server http://127.0.0.1:8099 -user demo@example.com -ping 5
```

Expect every step `ok`: `options`, `provision`, `settings`, `foldersync`,
`sync`, `directory` (the own user is found), `ping`. Run it a second time:
`provision` is `ok (not needed)` and `sync` is incremental.

**3. The exporter.** Save as `demo-config.json`:

```json
{
  "listen": "127.0.0.1:9116",
  "min_interval_seconds": 5,
  "targets": [
    {
      "name": "mock",
      "server": "http://127.0.0.1:8099",
      "user": "demo@example.com",
      "password_env": "EAS_DEMO_PASSWORD"
    }
  ]
}
```

```
EAS_DEMO_PASSWORD=secret ./eas-blackbox-exporter -config demo-config.json
curl 'http://127.0.0.1:9116/probe?target=mock'
curl 'http://127.0.0.1:9116/metrics'
```

Look for `eas_probe_success 1`, the `eas_probe_step_duration_seconds{step=...}`
lines and `eas_probe_directory_self_found 1`. Optional: point a Prometheus at it
with `deploy/prometheus.yml` (target name `mock`).

**4. The load worker** (terminal 4): 5 users with 2 devices each, metrics on 9117:

```
./eas-load -herd http://127.0.0.1:8788 -server http://127.0.0.1:8099 \
          -clients 5 -devices 2 -profile iphone,ipad -metrics 127.0.0.1:9117
```

**5. Drive it** (terminal 5):

```
curl -X POST -d 'target=all&command=run&job=eas-probe' http://127.0.0.1:8788/command
curl http://127.0.0.1:8788/status                        # jobs_ok should reach 5

curl -X POST -d 'target=all&command=run&job=eas-sync' --data-urlencode 'params=think=2-5&ping=30' http://127.0.0.1:8788/command
curl -X POST -d 'target=all&command=run&job=eas-gal'  --data-urlencode 'params=mode=typeahead&think=3-8' http://127.0.0.1:8788/command
curl -X POST -d 'target=all&command=run&job=eas-mail' --data-urlencode 'params=to=self&think=3-6&wait=20' http://127.0.0.1:8788/command
curl http://127.0.0.1:9117/metrics | grep -E 'eas_load_(requests_total|clients|items_total|directory)'

curl -X POST -d 'target=all&command=pause&pause_seconds=10' http://127.0.0.1:8788/command
curl -X POST -d 'target=all&command=stop' http://127.0.0.1:8788/command
```

Every command replaces the previous one (the latest wins), so start the next
job when you want to change what the devices do. At the end `eas-load` prints a
summary and saves its state.

**6. See the failure paths.** Restart `eas-mock-server` with `-error-rate 0.2` and
`eas-load`/`eas-probe` show errors counted by class. Probe with a wrong password:

```
EASPROBE_PASSWORD=wrong ./eas-probe -server http://127.0.0.1:8099 -user demo@example.com
```

The report ends with the `!! login refused` hint; the exporter pauses that
target for 10 minutes (`eas_probe_auth_backoff 1`) and `eas-load` ends the job at
once: a refused login is never retried in a loop.

**7. All of it automatically**

```
tests/e2e.sh .
```

starts both programs and `eas-load` with 3 users × 2 devices on loopback ports,
runs probe, sync, directory, pause and stop, and checks the numbers.

## End to end B: with Traveler

The same pieces, now against a real Traveler. Go in small steps; each one needs
the previous one to be green.

**Start with the probe:** the round trip script begins with `eas-probe` (all devices; with `-Mail` also a test
mail round trip) and only goes on when it is clean. For the first contact with a real server run just
that part (read-only):

```
.\scripts\roundtrip.ps1 -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -password-file d:\nsh_lab_pwd.txt -only-probe
scripts/roundtrip.sh -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -password-file ~/.eas/pw -only-probe
```

and then, when it is green, the whole round trip with your account (one user, one device, read-only;
mail is only created when you ask for it: add `-Mail` / `-mail` for a test mail to yourself that is
deleted again, and `-Devices 3 -Profile iphone,ipad` for several devices):

```
.\scripts\roundtrip.ps1 -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -password-file d:\nsh_lab_pwd.txt
scripts/roundtrip.sh -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -password-file ~/.eas/pw
```

**Before you start**

* The ActiveSync name of the Traveler server (or its proxy), and a way to reach
  it from the machine that runs the tools (`curl -i -X OPTIONS ...`, see above).
* Test users in Domino that are Traveler users: a mail file, an internet address
  and a known password. For the load test you need as many as the clients you
  want; the coordinator creates the *names* (`load000001@lab.dnug.eu` ...), the
  users must exist in Domino with those names and the password.
* Know the safety nets of the environment: account lockout, nginx rate limits,
  CrowdSec/fail2ban. Repeated failed logins are what gets a user or an address
  blocked (it happened in the lab): the tools back off, but check the password
  first.
* A Traveler device limit per user or device approval would show up as `403`
  or an approval message on the first run.

**1. One probe of your own account**

```
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -password-file d:\nsh_lab_pwd.txt -debug
```

All steps `ok`. The device appears in Traveler's device list as
`Apple-iPhone13C3-easprobe` (the default `iphone` profile). Then the held-Ping test,
with a heartbeat longer than your proxy's default timeout:

```
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -password-file d:\nsh_lab_pwd.txt -ping 120
```

**2. Monitoring**: the exporter with a real target in `config.json`:

```json
{
  "listen": ":9116",
  "state_file": "/data/state.json",
  "targets": [
    {
      "name": "traveler-lab",
      "server": "traveler.lab.dnug.eu",
      "user": "nsh@lab.dnug.eu",
      "password_file": "/run/secrets/traveler_lab_password"
    }
  ]
}
```

```
./eas-blackbox-exporter -config config.json
curl 'http://localhost:9116/probe?target=traveler-lab'
```

Then scrape it with `deploy/prometheus.yml` and load
`deploy/eas_probe_rules.yml` for the alerts (probe failed, login refused, held Ping
failed, slow sync, stale result, exporter down). A container build is in
`container/` (see `docs/build.md`) and `docker-compose.yml` (standalone: `docker compose up -d`).

**3. The coordinator with Traveler-ready accounts** (nshtestherd, your own
build): generate the account names for your domain and password, or use your
own CSV (`firstname,lastname,password,shortname,internetaddress`):

```
export NSHTEST_TOKEN=$(openssl rand -hex 24)        # recommended, workers need it too
./nshtestherd -bind 0.0.0.0 -generate 20 -domain lab.dnug.eu -password 'the-test-password'
./nshtestherd -bind 0.0.0.0 -csv users.csv
```

**4. The smallest load: one user, one device, one probe job**

```
export NSHTEST_TOKEN=...   # the same value
./eas-load -herd http://coordinator:8788 -server traveler.lab.dnug.eu -clients 1 -devices 1
curl -H "Authorization: Bearer $NSHTEST_TOKEN" -X POST -d 'target=all&command=run&job=eas-probe' http://coordinator:8788/command
curl -H "Authorization: Bearer $NSHTEST_TOKEN" http://coordinator:8788/status
```

`jobs_ok=1` and a new device in the device list of that user mean the whole
chain works: coordinator, worker, login, provisioning, sync.

**5. Realistic activity**: sync like a phone, push, directory lookups:

```
curl ... -d 'target=all&command=run&job=eas-sync' --data-urlencode 'params=think=10-30' http://coordinator:8788/command
curl ... -d 'target=all&command=run&job=eas-sync' --data-urlencode 'params=ping=60' http://coordinator:8788/command
curl ... -d 'target=all&command=run&job=eas-gal'  --data-urlencode 'params=mode=typeahead&think=10-30' http://coordinator:8788/command
curl ... -d 'target=all&command=run&job=eas-mail' --data-urlencode 'params=to=self&think=60-180&wait=120' http://coordinator:8788/command
```

Send mail to the test users (or let `domlem` do it) to see `eas_load_items_total`
grow and the push wake-ups.

**6. Scale up in steps.** Raise one dimension at a time and watch Traveler and
Domino: clients (`-clients 10`, `50`, ...), devices per user (`-devices 3
-profile iphone,ipad`), several workers in parallel from one coordinator (a
different `-name` each, e.g. `-name w1`, `-name w2`, each with its own state
file and `-metrics` port). Use `-ramp 1s` so the logins do not arrive in one
burst. Command line examples for all of this are in [LOADTEST.md](LOADTEST.md) and [PROBE.md](PROBE.md).

**7. Stop and clean up**

```
curl ... -X POST -d 'target=all&command=stop' http://coordinator:8788/command
```

* The workers report `done` and end; their state is saved
  (`<user config dir>/easprobe/load-<name>.json`).
* The test devices stay in Traveler's device list: delete them there (names like
  `Apple-iPhone13C3-easprobe`, `Apple-iPad13C1-easprobe`, `easprobe`).
* Delete the state files if you want the next run to register the devices again.

**Rules of thumb:** never loop a probe or a load job against a refused login;
start with 1 client and 1 device; keep the exporter's `min_interval_seconds` at
60 or more for real servers; keep `NSHTEST_TOKEN` set when the coordinator is
reachable from other machines.

## Metrics cheat sheet

| Metric                                                       | From      | Meaning                                                                                                        |
| ------------------------------------------------------------ | --------- | -------------------------------------------------------------------------------------------------------------- |
| `eas_probe_success`                                          | exporter  | 1 if every step of the probe succeeded                                                                         |
| `eas_probe_step_success{step}`, `..._duration_seconds{step}` | exporter  | per step: ok and time                                                                                          |
| `eas_probe_failure{step,class}`                              | exporter  | the failed step and the error class                                                                            |
| `eas_probe_directory_self_found`                             | exporter  | 1 if the own user was found in the address book                                                                |
| `eas_probe_mail_roundtrip_seconds`, `eas_probe_mail_lost`    | eas-probe | with `-mail`: time of the mail round trip to self, 1 if the mail did not come back                             |
| `eas_probe_mail_cleanup_failed`                              | eas-probe | with `-mail`: 1 if the test mail could not be deleted again (the subject is in the probe report)               |
| `eas_probe_mail_push_wake`, `eas_probe_mail_wake_seconds`    | eas-probe | only with `-mail-arrival push` (experimental, unreliable with one device): Ping result after the mail was sent |
| `eas_probe_auth_backoff`, `eas_probe_next_probe_seconds`     | exporter  | probing paused after a refused login / time to next probe                                                      |
| `eas_probe_probes_total`, `..._errors_total`                 | exporter  | the exporter's own counters (`/metrics`)                                                                       |
| `eas_load_requests_total{op,result}`                         | eas-load  | requests per ActiveSync command                                                                                |
| `eas_load_request_duration_seconds{op}`                      | eas-load  | latency histogram per command                                                                                  |
| `eas_load_clients{state}`, `eas_load_jobs_total`             | eas-load  | simulated users by state, jobs by result                                                                       |
| `eas_load_items_total{kind}`                                 | eas-load  | mail items seen by Sync                                                                                        |
| `eas_load_directory_self_total{result}`                      | eas-load  | own user found / missing in the address book                                                                   |
| `eas_load_mail_sent_total{to}`, `eas_load_mail_lost_total`   | eas-load  | mails sent by recipient kind; mails to self that did not arrive                                                |
| `eas_load_mail_arrival_seconds`                              | eas-load  | histogram: SendMail until the mail is in the Inbox (to self)                                                   |
| `eas_load_failures_total{class}`                             | eas-load  | failed steps by error class                                                                                    |
| `nshtestherd_*`                                              | herd      | the coordinator's own metrics (clients by state, jobs)                                                         |

Error classes: `mail_lost` (a sent mail did not come back), `auth` (401), `forbidden` (403), `not_found` (404),
`provision_required` (449), `server_error` (5xx), `eas_status` (an EAS status
code), `tls`, `dns`, `network`, `timeout`, `config`, `busy`.

## Troubleshooting

| Symptom                                         | Likely cause and what to do                                                                                                                                                                                                                                                                                                 |
| ----------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `401` on `options`, no `WWW-Authenticate`       | wrong login form or password; or the user/address is blocked. Check with curl, the Domino log, and the block lists; do not retry in a loop                                                                                                                                                                                  |
| `403`                                           | the user or device is not allowed (device rules, deny list, device limit)                                                                                                                                                                                                                                                   |
| `449`                                           | policy changed; the tools re-provision once by themselves                                                                                                                                                                                                                                                                   |
| `FolderSync` status 9 (`SyncKeyMismatch`)       | the server lost or recreated its state (for example the Traveler database was rebuilt) and no longer knows our stored keys. The probe rebuilds the folder hierarchy from sync key 0 by itself, as the protocol asks; the report shows `resync: hierarchy rebuilt (FolderSync status 9 ...)`. No need to delete `state.json` |
| `Sync` status 12, `Ping` status 7               | the folder hierarchy changed: the probe runs a FolderSync and retries once (`resync` in the report)                                                                                                                                                                                                                         |
| `Ping` status 5                                 | the heartbeat is outside the server's range; the response carries the allowed value, which the probe adopts (`heartbeat_adjusted_to`)                                                                                                                                                                                       |
| HTTP 502 / 503 from the proxy                   | the server behind the proxy (Traveler) is down or restarting: class `server_error`, not a login or protocol problem                                                                                                                                                                                                         |
| TLS error (`tls`)                               | private CA: use `-ca-file`; wrong host name in the certificate                                                                                                                                                                                                                                                              |
| `404`                                           | wrong path; the default is `/Microsoft-Server-ActiveSync`                                                                                                                                                                                                                                                                   |
| `timeout` on `ping`                             | a proxy cuts long requests: raise `proxy_read_timeout`; the step timeout is heartbeat + 30 s                                                                                                                                                                                                                                |
| Ping returns after about 55 s of 60 s           | normal for Traveler (answers slightly early)                                                                                                                                                                                                                                                                                |
| `sync` returns 0 items                          | an empty Inbox or none in the 2-week window; send a test mail                                                                                                                                                                                                                                                               |
| `directory` fails: own user not found           | the login is not an address (use `-directory-query`) or the address book does not list the user                                                                                                                                                                                                                             |
| `busy` in an exporter or probe result           | a probe of the same device is still running                                                                                                                                                                                                                                                                                 |
| `eas_load_mail_lost_total` grows                | mail to self does not come back within `wait`: slow routing, mail rules, or a full or broken mailbox; the mock server can show it with `-drop-mail`                                                                                                                                                                         |
| `eas_probe_mail_push_wake` is 0                 | only exists with `-mail-arrival push`, an experiment: with one device the `SendMail` aborts the held Ping (HTTP 409), so the value says nothing; use `eas_probe_mail_roundtrip_seconds`                                                                                                                                     |
| the exporter answers old data                   | the result cache: `min_interval_seconds`; `eas_probe_cache_age_seconds` shows the age                                                                                                                                                                                                                                       |
| `pool_exhausted` in `eas-load`                  | more clients than accounts in the coordinator: raise `-generate` / add CSV rows                                                                                                                                                                                                                                             |
| `eas_probe_mail_cleanup_failed` is 1            | the test mail could not be deleted and stays in the Inbox; its subject (`[easprobe ...]`) is in the probe report (`subject=`); delete it by hand and look at the trace                                                                                                                                                      |
| `HTTP 409 Conflict ... trying again` in the log | Traveler aborts a request when another request of the same device arrives; the tools wait and try again (up to 3 times); normal when two probes share a device id                                                                                                                                                           |
| state looks wrong after changes                 | delete the state file (or use `-fresh`) and let the devices register again                                                                                                                                                                                                                                                  |

### Traveler specifics found in the lab

What the wire traces against HCL Traveler showed, and what the tools do about it:

* **One open request per device.** A new request of the same device aborts a held Ping or Sync with
  `HTTP 409` and the header `X-Ibm-Traveler-Error-Message: Request is being aborted because a new
  request was received`. The tools treat a 409 as "try again", not as a failure. This is also why the mail
  test does not check push: with one device the `SendMail` always aborts the held Ping.
* **A delete Sync needs the same `Options` as the Inbox Sync** (filter type, body preference, window).
  Without them Traveler answers Sync status 3 (invalid sync key) whatever the key is. The probe sends its
  own delete request with the options.
* **The mail test touches only its own mail.** The subject is unique (`[easprobe DEVICE-RANDOM]`), the mail is
  found by that exact subject and deleted by its server id. Other mail in the Inbox is skipped and stays;
  an Inbox with thousands of mails needs more Sync round trips, nothing else. Leftovers of a failed cleanup
  are never swept up.
* **The device name** comes from the User-Agent text before the `/` and from `FriendlyName` in
  DeviceInformation. Both are the same text (`Apple-iPhone13C3-easprobe`), otherwise Traveler shows
  different names from run to run.
* **A held Ping ends after about 55 s of 60 s.** Normal for Traveler.

### Looking at the wire

`eas-probe -trace FILE` writes every request and response of a run to a file, with the WBXML bodies decoded
into readable XML. The EAS status codes are inside the bodies, so this is the way to see what a server
really answers. The file holds no password and no cookies (the `Authorization`, `Cookie` and `Set-Cookie`
headers are left out) but it does hold the account name and the text of the test mail. New runs are
appended.

```
eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -password-file pw.txt -mail -trace trace.txt
```

### Negative tests

Failures can be tested without breaking anything: start the mock server with `-sim` and let each request say how
the server should misbehave, with the header `X-Mock-Sim` (see [MOCKSERVER.md](MOCKSERVER.md), "Simulated misbehavior"). The
header is sent with `-header` (`eas-probe`, `eas-load`), with `headers` in an exporter target, or with `sim=...` in
the parameters of an `eas-load` job.

```
eas-mock-server -listen 127.0.0.1:8099 -sim
eas-probe -server http://127.0.0.1:8099 -user a@example.com -header "X-Mock-Sim: cmd=FolderSync,http=502"
```

The probe fails in the step `foldersync` with the error class `server_error` and exit code 1.
`cmd=Sync,status=3,count=1` fails once and shows the recovery. `tests/e2e.sh` runs a list of such cases
and checks the exit code and the error class of each.

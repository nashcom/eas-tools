# eas-tools

An ActiveSync (EAS) toolbox for HCL Traveler, written in Go on top of [go-activesync](https://github.com/hstern/go-activesync) (MIT).
It has three deliverables. They are separate programs that share their code, and each one works without the others.

## What is in the box

| #   | Deliverable                                  | What it is for                                                                                                  | Program                               | Jumpstart                     | Documentation                        |
| --- | -------------------------------------------- | --------------------------------------------------------------------------------------------------------------- | ------------------------------------- | ----------------------------- | ------------------------------------ |
| 1   | **Blackbox exporter** (the main deliverable) | Monitor Traveler with Prometheus: login, Provision, FolderSync, Sync, held Ping, a mail round trip, alert rules | `eas-blackbox-exporter`               | [below](#1-blackbox-exporter) | [docs/EXPORTER.md](docs/EXPORTER.md) |
| 2   | **Performance test**                         | Simulated users and devices against Traveler, and the one-command round trip of the whole chain                 | `eas-load`, coordinator `nshtestherd` | [below](#2-performance-test)  | [docs/LOADTEST.md](docs/LOADTEST.md) |
| 3   | **EAS client**                               | Read and send mail, calendar and contacts from the command line and through a REST API with a Swagger UI        | `eas-client`                          | [below](#3-eas-client)        | [docs/CLIENT.md](docs/CLIENT.md)     |

Supporting tools, and the rest of the documentation:

| What                                                                                        | Program           | Documentation                              |
| ------------------------------------------------------------------------------------------- | ----------------- | ------------------------------------------ |
| One probe on the command line (the engine of the exporter)                                  | `eas-probe`       | [docs/PROBE.md](docs/PROBE.md)             |
| A stand-in ActiveSync server: try everything without Traveler                               | `eas-mock-server` | [docs/MOCKSERVER.md](docs/MOCKSERVER.md)   |
| How the tools work together, two walk-throughs (without and with Traveler), troubleshooting |                   | [docs/GUIDE.md](docs/GUIDE.md)             |
| HTTPS of the exporter and the client: certificates, the micro CA, ports                     |                   | [docs/TLS.md](docs/TLS.md)                 |
| Build, test, source layout, validation status, known limits                                 |                   | [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) |

## Before you start

```
git clone https://github.com/nashcom/eas-tools.git && cd eas-tools
./compile.sh            # the programs for Linux amd64 in bin/linux-amd64/ (other platforms: see its usage text); needs Docker
./build.sh              # the container image nashcom/eas-tools with the exporter, the probe, the load worker and the client
docker pull ghcr.io/nashcom/eas-tools:latest   # or the finished image, for amd64 and arm64, from the first release on
```

- **Passwords** never go on a command line: they come from a file, from the environment or from stdin.
- **Options** have one dash: `-server`, `-password-file`. The order of precedence is the same in every program: command line, environment
  variable (a prefix for each program: `EASPROBE_`, `EASEXPORTER_`, `EASLOAD_`, `EASCLIENT_`), configuration file, default.
- **Secure by default.** The exporter and the client serve HTTPS unless you choose plain HTTP, and plain HTTP is on the loopback interface
  unless you name another address. They find a certificate in `/run/secrets/tls` or make one with a small CA of their own, which is for a
  try ([docs/TLS.md](docs/TLS.md)). The client only reads until you allow writing and sending with two separate switches.

### The ports

| Port   | Program                 | What                                                                                                |
| ------ | ----------------------- | --------------------------------------------------------------------------------------------------- |
| `9117` | `eas-blackbox-exporter` | HTTPS: `/probe`, `/metrics`, `/health`, `/debug/state`, `/config/reload` (the default)              |
| `9116` | `eas-blackbox-exporter` | plain HTTP, with `allow_http`: on `127.0.0.1` unless you name an address                            |
| `9100` | `eas-blackbox-exporter` | the metrics port: plain HTTP, open, `/metrics` and `/health` only                                   |
| `8443` | `eas-client`            | HTTPS: the REST API, `/openapi.json`, `/docs` (the default; `8444` is its health port, on loopback) |
| `8080` | `eas-client`            | plain HTTP, with `-allow-http`: on `127.0.0.1` unless you name an address                           |
| `8445` | `eas-mcp`               | HTTPS: the MCP endpoint `/mcp` with `-transport http` (`8446` is its health port, on loopback)      |
| `8081` | `eas-mcp`               | plain HTTP, with `-allow-http`: on `127.0.0.1` unless you name an address                           |
| `8788` | `nshtestherd`           | the coordinator of the load test                                                                    |
| `8099` | `eas-mock-server`       | the stand-in ActiveSync server (plain HTTP, on loopback)                                            |

All of them are above 1024, so the containers run as a non-root user with every capability dropped. Change a port on the outside:
`-p 443:9117`, a Kubernetes Service from 443 to 9117.

## 1. Blackbox exporter

Prometheus scrapes `GET /probe?target=NAME`; the exporter runs an ActiveSync probe of one configured account and answers with metrics:
whether the login worked, how long each step took, which step failed. It can also send a mail to the account itself and time the round trip,
and watch mail to an address outside (the mail monitor).

```
cp deploy/exporter-local.example.json config.json       # edit the target: server, user, password_file
./bin/linux-amd64/eas-blackbox-exporter -config config.json
curl 'http://127.0.0.1:9116/probe?target=traveler-lab'
```

Or as a container, with HTTPS (the first start makes a certificate and says so in the log):

```
cp deploy/exporter-container.example.json config.json   # edit the targets and the passwords
chmod 600 config.json
docker compose up -d --build
docker compose logs -f
```

Next: the scrape jobs in `deploy/prometheus.yml`, the alert rules in `deploy/eas_probe_rules.yml`, the configuration file, the mail monitor and
the endpoints in [docs/EXPORTER.md](docs/EXPORTER.md); the certificates in [docs/TLS.md](docs/TLS.md).

## 2. Performance test

`eas-load` is a worker of the coordinator [nshtestherd](https://github.com/nashcom/nshtestherd): one process hosts many simulated users with
several devices each, and the coordinator hands out the accounts and starts and stops the jobs (probe, directory lookups, sync, push, mail).
The round trip script runs the whole chain and checks it in one command: first one probe of the account (if it fails, nothing else starts),
then the coordinator, the workers and the jobs. It needs nshtestherd 0.9.2 or newer: from that version its options have one dash, like everything here.

```
scripts/roundtrip.sh                                    # the demo: the mock server and an nshtestherd container, on this machine (needs Docker)

# a real server, your account only, small and gentle: first only the probe (read-only)
scripts/roundtrip.sh -server traveler.example.com -user alice@example.com -password-file ~/.eas/pw -only-probe
```

On Windows the same script, with the same options, is `scripts\roundtrip.ps1`. Mail is created on a real server only when you ask for it with `-mail`. Direct use of
`eas-load`, its jobs and the standalone mode without a coordinator are in [docs/LOADTEST.md](docs/LOADTEST.md).

## 3. EAS client

A mail, calendar and contact client for the command line and a REST API that print the same JSON. It reads; changing the mailbox needs
`-write` and sending needs `-send`.

```
export EASCLIENT_PASSWORD='...'
eas-client -server traveler.example.com -email alice@example.com folders
eas-client -server traveler.example.com -email alice@example.com list inbox
eas-client -server traveler.example.com -email alice@example.com get inbox 5:12

# the REST API with the page of the documentation (Swagger UI), as a container
cp deploy/eas-client-container.example.json client.json   # the account, the token, the time zone
docker compose -f docker-compose.client.yml up -d         # then https://127.0.0.1:8443/docs
```

The commands, the REST routes, calendar invitations, contacts as vCard, the security of the API and the container are in
[docs/CLIENT.md](docs/CLIENT.md).

## The supporting tools

```
export EASPROBE_PASSWORD='...'
./eas-probe -server traveler.example.com -user alice              # one probe: the same steps as the exporter, on the command line
./eas-mock-server -listen 127.0.0.1:8099 -mail-every 10s          # no Traveler needed: point any of the tools at http://127.0.0.1:8099
```

[docs/PROBE.md](docs/PROBE.md) has the options, the steps, the output and the device identity; [docs/MOCKSERVER.md](docs/MOCKSERVER.md) the
stand-in server, its options and the simulated misbehavior for negative tests.

---

Copyright Nash!Com, Daniel Nashed. Apache 2.0, see [LICENSE](LICENSE). The protocol library is [go-activesync](https://github.com/hstern/go-activesync) (MIT).

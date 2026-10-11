# Building, layout and validation of eas-tools

[Back to the overview](../README.md).


Contents: [Quick reference](#quick-reference) | [Versions, releases and the container images](#versions-releases-and-the-container-images) | [Go and dependency versions](#go-and-dependency-versions) | [Build and test](#build-and-test) | [Layout](#layout) | [Validation status](#validation-status) | [Known limits](#known-limits)

## Quick reference

```
go build -o eas-probe ./cmd/eas-probe          # likewise eas-blackbox-exporter, eas-load, eas-mock-server
go test ./...
scripts/update.sh                            # latest Go and dependencies, then vet and test
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o easload-linux-amd64 ./cmd/eas-load
```


## Versions, releases and the container images

**One version for everything.** `Version` in `probe/profile.go` is the version of all the programs: `eas-blackbox-exporter`, `eas-probe`,
`eas-load`, `eas-client` and `eas-mock-server` print it with `-version`, and it is the version in their logs and in the User-Agent marker. A
test fails if a program gets a version of its own. The form is `X.Y.Z`; the first version is 0.9.0.

**Making a release.**

1. Raise `Version` in `probe/profile.go`.
2. Add a section `## X.Y.Z` to `CHANGES.md` with what changed. It becomes the text of the GitHub release.
3. Commit and push that.
4. Run `./push-release.sh`. It checks the version, that the section exists, that everything is committed and that the tag does not exist yet
   (`-force` moves a tag, which builds the images of that version again), writes `version.txt`, and pushes the tag `vX.Y.Z`.

The tag starts the workflow `.github/workflows/release.yml`, which checks that the tag is the version of the program, runs `go vet` and the
tests, builds the images and makes the GitHub release.

**The images** are built for **amd64 and arm64** in one go and pushed to `ghcr.io/nashcom/eas-tools`, as a manifest list, so that a pull
gets the right architecture:

| Image                                    | Base                        | Tags                        |
| ---------------------------------------- | --------------------------- | --------------------------- |
| `ghcr.io/nashcom/eas-tools`              | Alpine                      | `X.Y.Z` and `latest`        |
| `ghcr.io/nashcom/eas-tools:X.Y.Z-wolfi`  | Wolfi                       | `X.Y.Z-wolfi` and `wolfi`   |
| `ghcr.io/nashcom/eas-tools:X.Y.Z-static` | no shell, only the programs | `X.Y.Z-static` and `static` |

The Dockerfiles cross-compile the Go programs on the machine that builds (`--platform=$BUILDPLATFORM`, `GOARCH=$TARGETARCH`), so the arm64
image does not need an emulated compiler. The first push of a package to GHCR is private: set the package to public in its settings on GitHub
(the label `org.opencontainers.image.source` has linked it to this repository).

**Continuous integration** (`.github/workflows/ci.yml`) runs on every push and pull request: `gofmt`, `go vet`, the unit tests, and a build of
the image for amd64 and arm64 that is not pushed. To try the multi-platform build on your machine:
`docker buildx build --platform linux/amd64,linux/arm64 -f container/dockerfile .`

## Go and dependency versions

`go.mod` always names exact versions (Go cannot say "latest"), but the project
is kept on the newest ones: `scripts/update.sh` raises the `go` line to the
latest Go release, updates every dependency, runs vet and all tests, and restores
`go.mod`/`go.sum` if anything fails. The container build uses `golang:alpine` (the newest release).
Currently Go 1.27.2 with `go-activesync` v1.1.0 (the latest). Builds are not
reproducible over time that way; keep `go.sum` and note the Go version of released
binaries (`go version -m <binary>`) if you need to rebuild exactly.

## Build and test

The tests run in a Linux Go container, so the result does not depend on the machine:

```
./test.sh               # format check, vet, unit tests and the end-to-end test
./test.sh -unit         # only the fast part
./test.sh -all          # also the round trip (probe, coordinator, load worker)
```

Every check prints `[ OK ]` or `[ FAIL ]`, and the last line says whether everything passed (exit code 0 or 1).
Nothing there talks to a real server: the tests start the mock server inside the container and the nshtestherd container next to it.
The first run downloads the Go toolchain and modules into a Docker volume (`eas-tools-gocache`). Without Docker,
`tests/all.sh` does the same on a Linux or WSL machine that has Go, bash and curl.

The end-to-end test uses the real [nshtestherd](https://github.com/nashcom/nshtestherd) as coordinator: `./test.sh`
starts its container next to the test container (`tests/compose.yml`; image `nshtestherd:latest`, or set
`NSHTESTHERD_IMAGE`, for example `ghcr.io/nashcom/nshtestherd:latest`). With `HERD_URL=http://host:8788` a running
coordinator is used instead, local or remote. `./test.sh -unit` needs no coordinator.

Building the programs by hand:

```
go build -o eas-probe ./cmd/eas-probe
go build -o eas-blackbox-exporter ./cmd/eas-blackbox-exporter
go build -o eas-load ./cmd/eas-load
go build -o eas-mock-server ./cmd/eas-mock-server
go test ./...
./compile.sh -all                  # all platforms into bin/linux-amd64, bin/linux-arm64, bin/win-amd64, built in a Go container
```

The tests run against a small mock EAS server built from the library's own
WBXML codec. What has run against a real Traveler is listed under
[Validation status](DEVELOPMENT.md#validation-status).

## Layout

```
cmd/eas-probe/                CLI (one probe, text/JSON/Prometheus output, -trace)
cmd/eas-blackbox-exporter/   Prometheus blackbox-style HTTP exporter (the container's main program)
cmd/eas-load/                 nshtestherd worker generating ActiveSync load
cmd/eas-mock-server/         mock ActiveSync server for demos and tests
cmd/eas-client/              mail, calendar and contact client: command line and REST API (serve)
client/                     the operations of the client and the REST API (route table, OpenAPI document, Swagger UI page)
herd/                        client for the nshtestherd coordinator protocol
load/                        worker: client state machine, jobs, statistics
exporter/                    config file, /probe, /metrics, /health, result cache
mailmonitor/                  mail monitor: mail to an address outside, round-trip time, one thread for all targets
internal/easmockserver/      the mock ActiveSync server as a package (tests and cmd/eas-mock-server)
internal/testherd/           a tiny in-process coordinator for the Go unit tests
internal/certreload/        the certificate of a listener: reload while running, the micro CA, the log of the start
internal/ipacl/             allow lists of addresses, networks and ranges (allow_from)
internal/listenaddr/        the ports of a program (main, metrics, health) and the line that says what each one is
internal/settings/          command line, environment and file as one set of settings
internal/logging/           log level and format (text, JSON in the form of OpenTelemetry)
container/                   Dockerfiles (Alpine, Wolfi, static)
build.sh, compile.sh         container image build; binaries per platform into bin/ (see docs/build.md)
bin/                         built programs, one folder per platform (README.md placeholders only in git)
docker-compose.yml           the exporter standalone as a container; its targets are in config.json (not committed)
deploy/                      Prometheus scrape config, alert rules, examples (JSON, env files, compose) for all programs
test.sh, tests/all.sh        all tests in a Go container: format, vet, unit tests, end-to-end (with a real nshtestherd container), round trip
tests/compose.yml            the test container next to the nshtestherd container
container/dockerfile_test    the test image (Go image plus bash and curl)
tests/e2e.sh                 end-to-end test of the real binaries on loopback
scripts/roundtrip.*          round trip: eas-probe first, then coordinator, eas-load and jobs
scripts/update.sh            latest Go and dependencies, then vet and test
README.md                    the project: the three deliverables, the jumpstart of each, the ports
docs/EXPORTER.md            deliverable 1: the Prometheus blackbox exporter
docs/LOADTEST.md            deliverable 2: eas-load, the coordinator, the round trip script
docs/CLIENT.md              deliverable 3: eas-client, command line and REST API
docs/PROBE.md               eas-probe, the probe on the command line
docs/MOCKSERVER.md          eas-mock-server, the stand-in server
docs/GUIDE.md                how the tools work together, server setup, walk-throughs
docs/TLS.md                  HTTPS of the exporter and the client
docs/DEVELOPMENT.md          building, layout, validation status, known limits (this file)
docs/build.md                building the container image
probe/                       Session (one device: steps as methods) and Run(ctx, Config) -> Result
  probe.go                     steps, detection, error classification
  session.go, delete.go        the steps; the delete Sync that Traveler accepts
  mail.go                      mail round trip, find and delete only the own test mail
  transport.go, trace.go       per-request timing (DNS, connect, TLS, ...), 409 retry, wire trace
  state.go                     in-memory Store of devices, periodic JSON snapshot, per-device lock
  status.go                    EAS status names per command
  render.go, prom.go           text report, Prometheus output
```

## Validation status

What has really run, and against what (the lab is `traveler.lab.dnug.eu`, HCL Traveler behind nginx):

* **Against the real Traveler (2026-10-09), with `eas-probe`:** OPTIONS (EAS 2.5 to 16.1 offered, 14.1 used),
  Provision (two phases, no policy), Settings (DeviceInformation), FolderSync, Sync (empty Inbox, later
  incremental runs without provisioning again), a held Ping (answered after about 55 s of 60 s),
  and three devices of one user (`iphone`, `ipad`, `easprobe` profiles) in the device list. Basic
  authentication works; a blocked user showed up as `401` without `WWW-Authenticate`.

* **Against the real Traveler (2026-10-09), later runs:** the directory (GAL) lookup finds the own user; the
  mail round trip (SendMail to self, found by Sync, deleted again) works with `eas-probe -mail`; the
  exporter's `/probe`, `?mail=1` and `/metrics` answered with curl; `eas-probe -trace` shows the wire
  traffic. Findings from the traces: Traveler allows one open request per device and aborts a held Ping with
  HTTP 409 when another request of the device arrives (the probe retries a 409), and a delete Sync has to
  repeat the `Options` of the Inbox Sync or Traveler answers Sync status 3 whatever the key.

* **Push is not tested.** With one device the `SendMail` aborts the held Ping, so the result proves nothing.
  The mail round trip uses polling by default; `-mail-arrival push` is an experiment.

* **Status codes:** EAS status numbers mean different things per command (FolderSync 9 is a sync key
  mismatch, Sync 9 a disk problem). The tools name them with the MS-ASCMD table of the command, not the
  library's shared one, and recover as the protocol asks: FolderSync 9 restarts the hierarchy at sync key 0,
  Sync 12 and Ping 7 run a FolderSync and retry, Ping 5 adopts the heartbeat limit the server returns.

* **Not yet against the real Traveler:** a Ping longer than 60 s through the proxy, the exporter with
  Prometheus scraping it, `eas-load` with `nshtestherd`, and the container build.

* **Against the mock server and in unit tests:** everything above, including the mail round trip,
  lost mail, damaged state file, auth backoff, and several devices per account
  (`tests/e2e.sh`, `scripts/roundtrip.*`).

## Known limits

* Sends mail only when asked (`-mail`, `-mail` / `-Mail`, the `eas-mail` job, `mail=1` on an
  enabled exporter target); never acknowledges a remote wipe request.

* The mail test is cleaned up and measured only for mail to the account itself; mail to
  other users stays in their mailboxes.

* A second probe of the same device while one is running is refused (error class `busy`).

* Device approval flows and real policy documents are untested (the lab has no policy).

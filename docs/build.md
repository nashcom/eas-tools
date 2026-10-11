# Building the Container Image

## TL;DR

Run the default build on Linux (or in WSL):

`./build.sh`

Use additional options for alternative images:

`./build.sh -wolfi`
`./build.sh -static`

Run it:

```bash
docker run --rm -p 9116:9116 -v "$PWD/config.json":/etc/eas-tools/config.json:ro nashcom/eas-tools
```

Standalone with Docker Compose (the targets are in `config.json`, copied from `deploy/exporter-container.example.json`, the password is a Docker secret):

```bash
docker compose up -d
```

---

# Overview

The image contains three programs: the exporter (`eas-blackbox-exporter`, the entry point), the command line probe
(`eas-probe`) and the load worker (`eas-load`). The two others are in `/usr/local/bin` and can be started with
`--entrypoint`.

Each variant uses a **multi-stage build**:

1. A **Go builder image** compiles the programs (static, `CGO_ENABLED=0`).
2. The binaries are copied into a **minimal runtime image**.

The runtime image runs as an unprivileged user (uid 1000) and holds no build tooling and no source code. The only
writable path is `/data`, where the exporter keeps `state.json` (the registered devices). Mount a volume there.


Standalone with Docker Compose (targets in `config.json`, see `docker-compose.yml`; for one target with only an env file see `deploy/exporter-env.compose.example.yml`):

```bash
docker compose up -d
```

---

# Available Build Variants

| Variant              | Dockerfile                    | Builder Image                  | Runtime Image                      | Description                                    |
| -------------------- | ----------------------------- | ------------------------------ | ---------------------------------- | ---------------------------------------------- |
| **Alpine (default)** | `container/dockerfile`        | `golang:alpine`                | `alpine:latest`                    | Standard image with a shell for debugging.     |
| **Wolfi**            | `container/dockerfile_wolfi`  | `cgr.dev/chainguard/go:latest` | `cgr.dev/chainguard/wolfi-base`    | Chainguard Wolfi base, optimized for few CVEs. |
| **Static**           | `container/dockerfile_static` | `cgr.dev/chainguard/go:latest` | `cgr.dev/chainguard/static:latest` | Smallest image: only the binaries, no shell.   |

The images are tagged `nashcom/eas-tools` (Alpine), `nashcom/eas-tools:wolfi` and `nashcom/eas-tools:static`. The released images, for amd64 and arm64, are `ghcr.io/nashcom/eas-tools` with the same tags (see DEVELOPMENT.md, "Versions, releases and the container images").

---

# Files

| File                             | Purpose                                                                                                                |
| -------------------------------- | ---------------------------------------------------------------------------------------------------------------------- |
| `build.sh`                       | builds the container image (selects the Dockerfile)                                                                    |
| `compile.sh`                     | builds the programs on demand into `bin/<platform>/` (`linux-amd64`, `linux-arm64`, `win-amd64`) in a Go container     |
| `test.sh`                        | runs all tests in a Go container (format, vet, unit tests, end-to-end test, optionally the round trip)                 |
| `tests/all.sh`                   | the test steps; runs inside the test container, or directly on a Linux machine with Go, bash and curl                  |
| `tests/compose.yml`              | the test container next to a real `nshtestherd` container for the end-to-end test (`NSHTESTHERD_IMAGE`, `HERD_URL`)    |
| `.dockerignore`                  | lets only the Go sources into the build context: no lab configs, state, traces, secrets or programs                    |
| `.gitignore`, `bin/*/.gitignore` | keep lab settings, secrets, state and built programs out of the repository; each `bin` folder ignores its own binaries |

---

# Notes

* The build uses `--no-cache`, so every build takes the newest Go and the newest base images.
* `go.mod` is kept at the newest Go version by `scripts/update.sh`. `golang:alpine` is the newest release; if it is
  older than `go.mod`, the Go tool downloads the toolchain during the build.
* The health check calls `eas-blackbox-exporter -health-check`, so it also works in the static image without a shell.
* Secrets (password files, the token) are mounted, never built into the image. See `docker-compose.yml`.

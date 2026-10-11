# HTTPS for the listeners

The exporter (`eas-blackbox-exporter`) and the client (`eas-client serve`) can serve HTTPS. Both use the same code
(`internal/certreload`) and behave the same way. TLS is secure by default: with a certificate the main port speaks HTTPS and nothing else, and
without one plain HTTP has to be chosen on purpose (`allow_http`) unless the listener is on loopback. The metrics of the exporter are on a
port of their own, plain HTTP and open by default, because they carry no secret; see "Ports and plain HTTP".

This page is about the **listener**, the side that Prometheus or your own program connects to. The TLS of the connection to
Traveler (`insecure`, `ca_file`) is a different thing and is described in [GUIDE.md](GUIDE.md#specifying-a-traveler-server).

Contents: [The short way](#the-short-way) | [Settings](#settings) | [Ports and plain HTTP](#ports-and-plain-http) | [What the start tells you](#what-the-start-tells-you) | [What happens at the start](#what-happens-at-the-start) | [A renewed certificate](#a-renewed-certificate) | [The default certificate, and the one that the program makes](#the-default-certificate-and-the-one-that-the-program-makes) | [Prometheus](#prometheus) | [Containers](#containers) | [Troubleshooting](#troubleshooting) | [What this does not do](#what-this-does-not-do)

## The short way

Nothing to configure. A program that is reachable from the network serves HTTPS, and when it finds no certificate it makes one:

```
docker compose up -d            # the exporter: HTTPS on 9117, the metrics on 9100
```

The certificate is looked for in **`/run/secrets/tls`** (`tls.crt`, `tls.key`, and `ca.crt`: the names of a Kubernetes secret of the type
`kubernetes.io/tls`, with the CA that cert-manager adds). If it is not there, the program makes it with a small CA of its own, writes the CA
(`ca.crt`, `ca.key`) and the certificate next to it, and says so in the log. That is for a try. For anything that other people use, put a
certificate of your own in that directory (or name it with `tls_cert` and `tls_key`): it is used as it is and never touched.

## Settings

| Exporter (`config.json`, `EASEXPORTER_`)       | Client (flag, `client.json`, `EASCLIENT_`)                 | Meaning                                                                                                                                                                      |
| ---------------------------------------------- | ---------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `tls_cert`                                     | `-tls-cert`, `tls_cert`                                    | the certificate file in PEM: the server certificate first, then the chain. Default: `/run/secrets/tls/tls.crt`                                                               |
| `tls_key`                                      | `-tls-key`, `tls_key`                                      | its private key in PEM. Default: `/run/secrets/tls/tls.key`                                                                                                                  |
| `tls_generate`                                 | `-tls-generate`, `tls_generate`                            | make a CA and a certificate when neither file is there, at the files that you name; not needed for the default ones, which are made anyway                                   |
| `tls_valid_days` (default `365`, 1 to 3650)    | `-tls-valid-days`, `tls_valid_days`                        | how long a certificate that the micro CA makes is valid; it is made again at every start, and one of yours is not touched; never longer than the CA (ten years)              |
| `tls_names` (list, comma separated in the env) | `-tls-names` (repeatable), `tls_names`                     | names of the generated certificate: DNS names and IP addresses, added to `localhost`, `127.0.0.1` and `::1`; they replace the host name, which is in when no names are given |
| `tls_check_interval_seconds` (default `60`)    | `-tls-check-interval` (default `1m`), `tls_check_interval` | how often the files are looked at                                                                                                                                            |
| `allow_http`                                   | `-allow-http`, `allow_http`                                | serve everything over plain HTTP without a certificate, for a proxy that terminates TLS; an error together with `tls_cert`                                                   |
| `listen_metrics`                               | `-listen-health`, `listen_health` (TLS only)               | the second port: exporter, the metrics port (always, open); client, the `/health` port (with TLS, loopback). Default: the port after `listen`                                |
| `allow_from_metrics` (list)                    | `-allow-from-health` (repeatable), `allow_from_health`     | the peers that may connect to it; the client's health port needs the list when it is not on loopback                                                                         |

- `tls_cert` and `tls_key` go together. One without the other is an error at the start.
- `tls_generate` is for files that you name; it excludes `allow_http` and a loopback `listen`, where there is no TLS.
- `listen_health` and `allow_from_health` of the client need TLS: without it the one port has /health.
- The usual precedence applies: command line, environment, JSON file, default.
- TLS 1.2 and up; HTTP/2 is offered.

Without `tls_cert`, without `allow_http` and off the loopback the programs use the default certificate. With `allow_http` they
serve plain HTTP and warn that the traffic is not encrypted.

## Ports and plain HTTP

There are three rules, and they are the same for the exporter and the client:

1. **TLS is the default, and the main port then speaks HTTPS and nothing else.** There is no setting that opens it for plain HTTP next to
   it: `allow_http` together with a certificate is an error. A program that is reachable from the network and has no certificate named looks
   for one in `/run/secrets/tls` and makes one when there is none (see "The default certificate").
2. **Plain HTTP is a choice.** It needs `allow_http` (`-allow-http`, `EASEXPORTER_ALLOW_HTTP` / `EASCLIENT_ALLOW_HTTP`), for example behind
   a proxy that terminates TLS. On its own it means loopback (`127.0.0.1:9116`); a network address has to be named as well (`listen`). A **loopback** address (`127.0.0.1`, `::1`, `localhost`) needs no choice and gets no certificate: it never
   leaves the machine, so a local try, the tests and a proxy on the same host just work.
3. **The metrics port is open by default.** It carries no secret, and a scrape needs neither a token nor the overhead of TLS.
   `allow_from_metrics` restricts it.

| Port    | Setting                                                                                                                                             | Speaks                                             | Serves                                    | Who may connect                            |
| ------- | --------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------- | ----------------------------------------- | ------------------------------------------ |
| main    | `listen` (exporter default `:9117` with TLS and `127.0.0.1:9116` with `allow_http`, client `:8443` with TLS and `127.0.0.1:8080` with `allow_http`) | HTTPS; plain only with `allow_http` or on loopback | everything, `/metrics` as well            | `allow_from`                               |
| metrics | exporter: `listen_metrics`, default `:9100` on all interfaces                                                                                       | plain HTTP, always                                 | `/metrics` and `/health` and nothing else | everyone, or the list `allow_from_metrics` |
| health  | client, with TLS only: `listen_health`, default `127.0.0.1:` and the port after `listen` (`8444`)                                                   | plain HTTP                                         | `/health` and nothing else                | loopback, or the list `allow_from_health`  |

The exporter's **metrics port** exists with TLS and without. `/probe` (it carries the token), `/debug/state` and `/config/reload` are on
the main port only; on the metrics port they do not exist (`404`). `/metrics` is on the main port as well, so an existing scrape
keeps working. The **health port of the client** is only there with TLS, because then the main port cannot be asked for `/health`
without a certificate; the client has no metrics, and its API, `/docs` and `/openapi.json` are on the main port only.

The health check of the exporter (`-health-check`, the `HEALTHCHECK` of the image) uses the metrics port, so it needs no certificate
and no `--no-check-certificate`. In the client use `http://127.0.0.1:8444/health` when TLS is on.

The metrics port is **9100** (the port that `node_exporter` uses by convention, so on a host that runs it, map the port at Docker, or set
`listen_metrics`). If the port is taken, the start fails with the error of the system and a hint.

### The well-known ports, and 80 and 443

| Port   | What                                                                                                             |
| ------ | ---------------------------------------------------------------------------------------------------------------- |
| `9117` | the exporter with a certificate: HTTPS                                                                           |
| `9116` | the exporter with `allow_http` or on a loopback address: plain HTTP, on loopback unless you name another address |
| `9100` | the metrics port of the exporter: plain HTTP, open, `/metrics` and `/health`                                     |
| `8443` | the client with TLS (`8444` is its health port, on loopback)                                                     |
| `8080` | the client with `allow_http`: plain HTTP, on loopback unless you name another address                            |

All of them are above 1024, so the container needs nothing special: it runs as a non-root user with every capability dropped,
`no-new-privileges` and a read-only file system, and **binding a port below 1024 would need a privilege** that this setup does not
give. Do not make the program listen on 80 or 443 with `setcap` (the flag `no-new-privileges` blocks file capabilities, and the
capability would stay on the binary for everyone who uses the image) or with `cap_add: NET_BIND_SERVICE`. Change the port on the
**outside**, where it costs nothing: `ports: ["443:9117"]` in Docker, a Service from 443 to the container port 9117 in
Kubernetes, `proxy_pass https://eas-probe:9117` in NGINX. The same for the metrics port when `node_exporter` already uses 9100 on the
host: `ports: ["9101:9100"]`, and Prometheus scrapes `:9101`.

The main port is not tied to the mode: `listen` may be anything, and the defaults are only the ports that you can leave out.
`eas-load -metrics :9117` of the load worker uses the same number as the HTTPS port of the exporter; on a host that runs both,
give one of them another port.

### Which setup for which deployment

| Deployment                                                    | Main port                                                                                                                                                                                              | Metrics port                                                                                                          |
| ------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------- |
| Directly reachable, own certificate                           | `tls_cert`, `tls_key`; `allow_from` for who may use it                                                                                                                                                 | open, or `allow_from_metrics` with the address of Prometheus                                                          |
| **NGINX on the same host** (or in the same network namespace) | `listen: 127.0.0.1:9116`, plain: no certificate, no choice needed; NGINX does `proxy_pass http://127.0.0.1:9116` and terminates TLS                                                                    | `listen_metrics` open to Prometheus, or behind NGINX as well                                                          |
| **Kubernetes, behind an ingress NGINX**                       | **keep TLS**: `tls_cert` from the secret (cert-manager), the ingress re-encrypts to the pod (`nginx.ingress.kubernetes.io/backend-protocol: "HTTPS"`); the channel inside the cluster is protected too | open inside the cluster, `allow_from_metrics` for the network of Prometheus; the readiness and liveness probes use it |
| A proxy in another container that terminates TLS (Docker)     | `allow_http: true`, `listen: ":9116"` (unencrypted is loopback unless you name an address) and `allow_from` with the network of the proxy; the port is not published                                   | as above                                                                                                              |

The row for Kubernetes is the recommended one wherever a pod can have a certificate: it costs one secret, and the traffic between
the ingress and the pod (the token of `/probe`, the mail of the client) is no longer clear text on the cluster network. `allow_http`
is for the cases where that is not possible, and then `allow_from` should name the proxy, which the program says in its log.

### Scraping the metrics port

The scrape jobs of `/probe` use HTTPS (see below). The job for the exporter's own metrics can use the metrics port:

```json
"listen": ":9117",
"listen_metrics": ":9100",
"allow_from_metrics": ["10.1.2.0/24"]
```

The list is the address of Prometheus. Everybody else, also from the host and from the outside, gets `403`. The program says at the
start when the metrics port is open to everyone. In Docker do **not** publish the metrics port (`ports:`) unless the scraper is
outside Docker: Prometheus on the container network reaches it without. `deploy/prometheus.yml` has the job.

## What the start tells you

The log at the start says, for every port and for the certificate, what is there. Nothing has to be looked up in the
configuration to know what a running program does.

The start writes **one short line for each port**. It says what is enabled and with which security, and nothing else:

```
INFO started version=0.9.0 targets=[traveler-lab] mail_monitor=1 min_interval=1m0s state=/data/state.json devices=2 config="..."
INFO listener name=main addr=0.0.0.0:9117 proto=https from=10.0.0.0/8 auth="token(/probe /debug/state /config/reload) none(/metrics /health)"
INFO listener name=metrics addr=0.0.0.0:9100 proto=http from=open auth="none(/metrics /health)"
```

| Field   | Says                                                                                                                    |
| ------- | ----------------------------------------------------------------------------------------------------------------------- |
| `name`  | `main`, `metrics` (exporter) or `health` (client, with TLS)                                                             |
| `addr`  | where the socket really listens (`[::]` and `0.0.0.0` are all interfaces)                                               |
| `proto` | `https`, or `http`: not encrypted                                                                                       |
| `from`  | `open`, `loopback`, or the list of `allow_from` / `allow_from_metrics` (loopback is always in); everybody else gets 403 |
| `auth`  | what the paths need, grouped: `none(...)` or `token(...)`; `token-error` when a token is configured but cannot be read  |

Which paths a port has is the list in `auth`; everything else is a 404. A token configured for the exporter protects `/probe`,
`/debug/state` and `/config/reload`; `/metrics` and `/health` need none. The client's REST API is the path `/accounts/...`, and `/docs`
and `/openapi.json` are `none` with `-docs`, because a browser cannot send the header. Plain HTTP on a network address, chosen with
`allow_http`, is a warning of its own.

With `-verbose` (debug level) there is the explanation that does not belong in the normal log: a `listener detail` line for each port
(the interfaces, the TLS versions, why it is as it is: HTTPS only, plain because loopback, plain because `allow_http`, the address as it
was configured) and an `endpoint` line for each path (what it is, and the header and the source of the token).

**The certificate**, at the start and again at every renewal: one line for the files and one line for the **leaf**, the server certificate,
short, with the name of the root. What is empty is left out:

```
INFO TLS certificate loaded cert=/run/secrets/tls/tls.crt key=/run/secrets/tls/tls.key chain=2
INFO TLS certificate days=364 cn=eas.example.com dns="[eas.example.com localhost]" ips="[10.1.2.3 127.0.0.1 ::1]" key="ECDSA P-256" sig=ECDSA-SHA256 sha1=f9:c6:b3:... root="easprobe micro CA"
```

| Field        | Says                                                                                                                                      |
| ------------ | ----------------------------------------------------------------------------------------------------------------------------------------- |
| `days`       | the days left, first on the line because it is what matters most                                                                          |
| `cn`         | the common name of the certificate                                                                                                        |
| `dns`, `ips` | the names that the certificate is valid for; left out when there are none                                                                 |
| `key`, `sig` | the public key and the signature algorithm                                                                                                |
| `sha1`       | the SHA-1 fingerprint, the one that browsers and `openssl x509 -fingerprint` show                                                         |
| `root`       | the name of the root: the issuer of the last certificate of the file, which is the root itself, or the root that an intermediate leads to |
| `chain`      | how many certificates the file holds                                                                                                      |

The rest of the chain and the rest of each certificate (the issuer, the serial, the SHA-256, key usage and extended key usage, the
validity, the identifiers, and what each certificate is: leaf, intermediate or root) is at debug level (`-verbose`), one line for each
certificate. The log also warns when something looks wrong, one line each:

- the leaf is not meant for a server (no `ServerAuth`), or has no DNS name and no IP address (current clients refuse it);
- a certificate of the chain is not signed by the next one: the order in the file is wrong, or a CA is missing;
- a certificate has less than 14 days left, or the certificate is expired.

A key is never logged.

## What happens at the start

1. With the default certificate or `tls_generate`, a pair that is not there is made, and one that the micro CA made is made again (see "The
   default certificate"). With only one of the two files there, nothing is made and the start fails: that is a state that someone has to
   look at.
2. The pair is loaded. A pair that does not fit (a key of another certificate, a broken file) stops the start with the file names in
   the message. A certificate that is **expired** does not stop it: it is logged as a warning, so that the service is there and
   the operator sees why clients complain.
3. The log says what is served and where: see "What the start tells you" above. A certificate with less than 14 days left is logged as
   a warning.

## A renewed certificate

No trigger is needed. Replace the files and the listener serves the new certificate from the next connection on. Nothing is
restarted, no connection is dropped.

- **The watcher.** Every check interval the program looks at the time and size of both files. Links are followed, so the swap of a
  link to another directory (what Docker secrets, Kubernetes secrets and many ACME clients do) is a change as well.
- **The pause.** A tool that renews a certificate writes the certificate and the key one after the other. The program waits one
  second after it saw a change and loads both then.
- **The check of the new pair.** The certificate and the key must fit, and the certificate must not be expired. If they do not, the
  one in use stays, the failure is logged once and counted, and the broken pair is not tried again until the files change once more.
  A renewal that goes wrong is therefore a warning in the log, not an outage.
- **`SIGHUP`** loads the files at once, without waiting for the interval, and also when nothing seems to have changed.
- **The configuration check of the exporter** (the timer, `POST /config/reload` and `SIGHUP`) looks at the certificate files too,
  with the same cheap check. It does nothing when the files did not change.

The client has no configuration reload, so it has the watcher and `SIGHUP`.

A change of the **file names** (`tls_cert`, `tls_key`), of the switch to HTTPS or of an address (`listen`, `listen_metrics`) is not
applied by a reload; `allow_from` and `allow_from_metrics` are. The exporter says so in
the log and in `restart_needed` of its debug state; restart it.

## The default certificate, and the one that the program makes

When no certificate is named (`tls_cert`, `tls_key`), the program is not asked to serve plain HTTP (`allow_http`), and `listen` is not a
loopback address, the certificate is looked for in `/run/secrets/tls`. What the start does:

| What is in the directory                              | The start                                                                                                           |
| ----------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| `tls.crt` and `tls.key` of a CA of yours (or ACME)    | used as they are, never changed; a renewal is picked up while it runs (see above)                                   |
| `tls.crt` and `tls.key` that the micro CA made before | **both made again at every start**, with the same names, signed by the same CA                                      |
| nothing                                               | the micro CA and a certificate are made, a warning says so                                                          |
| only one of `tls.crt` and `tls.key`                   | in a directory of the micro CA: **both made again** (a warning says so). Otherwise an error, and nothing is touched |
| nothing, and the directory cannot be written          | the start is refused, and the message names the ways out                                                            |

What is *ours* is decided by names, so that a real certificate is never broken:

- a **certificate is ours** when its issuer is the CA called `easprobe micro CA`: this program made it, with this CA or with one that is gone;
- the **directory is ours** when `ca.crt` and `ca.key` are there, belong together, and are that CA.

A certificate of any other issuer is **never changed**, whatever is next to it: with no CA files, with the `ca.crt` that cert-manager puts
there (no `ca.key`), or with the files of our own CA. A CA in the directory that is not the micro CA is never used to sign anything, and
nothing is made next to it. **To get a new CA, remove `ca.key`** (or all the files of the directory) and restart: the `ca.crt` of the micro CA without its key is replaced by a
new CA, and the certificate is made again by it. The log says that there is a new CA, and the clients have to trust the new `ca.crt`. Only
a `ca.crt` called `easprobe micro CA` is replaced this way: the `ca.crt` of cert-manager or of your CA never is, and a certificate of another
issuer next to it is never changed. The pair is written together: both files go to temporary files first, so that a failure leaves the old pair, not a new
key with an old certificate.

The micro CA:
- a CA (`ca.crt`, `ca.key`, ten years) and a server certificate (`tls_valid_days`, 365 by default, never longer than the CA) in the same directory; the file `tls.crt` holds the server
  certificate and then the CA, as a server sends it;
- the key is ECDSA P-256 (RSA 4096 takes seconds to make);
- the CA is kept, and signs every certificate that is made again: a client that trusts `ca.crt` once keeps trusting;
- the names are `localhost`, `127.0.0.1`, `::1` and the host name. **`tls_names`** (`EASEXPORTER_TLS_NAMES=eas.example.com,10.1.2.3`, or
  `-tls-names` in the client) adds the names that the clients use and **replaces the host name**, which in a container is an id that
  nobody uses (a certificate for `localhost` is rejected for `eas.example.com`); the loopback names stay, so a check on the machine itself
  works. A change of the names takes effect at the next start, when a certificate of the micro CA is made again;
- the files are written atomically, the keys with the mode `0600`. The exporter and the client of a container may share the directory
  (the same volume): a lock keeps two programs that start at the same moment from making two CAs.

The directory is part of the image (the Dockerfiles create `/run/secrets/tls` for the non-root user). **Without a volume the certificate is
gone when the container is, and the next start makes a new CA**: clients that trusted the old one have to trust the new. That is fine for
a try; mount a volume on the directory to keep it, or a secret with your own certificate. With `read_only: true` and nothing mounted, the
directory cannot be written, and the start says so.

`tls_generate: true` (`-tls-generate`) does the same for files that **you** name: `tls_cert` and `tls_key` at a place of your choice. It
is not needed for the default place.

Hand `ca.crt` to the client that connects: `curl --cacert ca.crt https://host:9117/health`, for Prometheus `tls_config: {ca_file: ca.crt}`.
The micro CA is a convenience; it is not what you want in production, where the certificate comes from your own CA or from ACME.

## Prometheus

The exporter publishes the state of its certificate:

| Metric                                                  | Meaning                                                    |
| ------------------------------------------------------- | ---------------------------------------------------------- |
| `eas_probe_tls_certificate_not_after_timestamp_seconds` | Unix time when the certificate in use expires              |
| `eas_probe_tls_reloads_total`                           | certificates loaded while the listener ran                 |
| `eas_probe_tls_reload_errors_total`                     | changed files that could not be loaded; the old pair stays |

They are not there when the exporter has no certificate. `/debug/state` has a `tls` section with the same numbers plus subject,
issuer, names, key type, fingerprint, the files and the two listen addresses. Requests that either list refused count in
`eas_probe_denied_requests_total`.

The scrape job (`deploy/prometheus.yml` has the example):

```
  - job_name: eas_probe
    scheme: https
    tls_config:
      ca_file: /etc/prometheus/secrets/eas_ca.crt
      server_name: eas-probe.example.com     # the name in the certificate, if the target is an address
```

The alerts for an expiring certificate and for a renewal that went wrong are in `deploy/eas_probe_rules.yml`
(`ActiveSyncExporterCertificateExpires`, `ActiveSyncExporterCertificateReloadFailed`).

## Containers

- The files must be readable by the user of the container (the image runs as a non-root user). Mount the directory, not a single
  file: a renewed file is then seen. A Docker or Kubernetes secret mounted as a directory works, the swap of its link is seen.
- The default directory `/run/secrets/tls` is in the image. Mount a volume on it to keep the micro CA and its certificate (and to share
  them: the exporter and the client of a container can use the same volume), or a secret with your own certificate. Without a mount a
  new CA is made at every start of a new container, which is fine for a try.
- **The health check of the exporter** (`-health-check`, the `HEALTHCHECK` of the image) asks the metrics port on loopback: no
  certificate is involved, with TLS and without. It reads the address from the configuration, so a `listen_metrics` of your own is found.
- **The health check of the client** in `docker-compose.client.yml` asks the health port: `http://127.0.0.1:8444/health`
  when the HTTPS port is 8443. If you set `listen_health`, change the port there.
- **Publish the main port only.** `ports: ["8443:8443"]` is the encrypted one. The metrics port stays on the container network, where
  Prometheus reaches it; the health port stays inside the container. In Kubernetes: the pod port of the metrics port is for the
  scrape and the probes, and a `NetworkPolicy` can narrow it as well as `allow_from_metrics`.
- Combine TLS with `allow_from` (see [CLIENT.md](CLIENT.md)): TLS protects the content, the list says who may connect at all.
  The loopback interface is always allowed, on every port.

Examples: `deploy/exporter-container.example.json`, `deploy/eas-client-container.example.json`.

## Troubleshooting

| What you see                                                                                          | Meaning and what to do                                                                                                                                                |
| ----------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| start: `TLS certificate ... and key ...: tls: private key does not match public key`                  | the key belongs to another certificate: put the two files that belong together                                                                                        |
| start: `tls_cert and tls_key go together`                                                             | one of the two is empty                                                                                                                                               |
| start: `only one of them is there; not generating over it`                                            | a certificate without a key (or the other way round) where the program makes its own: look at the files, remove the leftover                                          |
| start: `there is no certificate in /run/secrets/tls and none can be made there`                       | mount a certificate on the directory (`tls.crt`, `tls.key`), or a volume that can be written, or set `tls_cert` and `tls_key`, or choose plain HTTP with `allow_http` |
| log: `no TLS certificate was there: one was made by the micro CA`                                     | the first start without a certificate: hand `ca.crt` to the clients, and use a certificate of your own for anything but a try                                         |
| log: `the generated certificate could not be renewed`                                                 | the directory cannot be written now: the certificate that is there is used; fix the mount before it expires                                                           |
| log: `the TLS certificate is expired`                                                                 | the start went on; renew the files and they are loaded within the interval                                                                                            |
| log: `TLS certificate reload failed: the certificate in use stays`                                    | the new files are broken, mismatched or expired; fix them: the next change is tried again                                                                             |
| log: `it expires soon`                                                                                | less than 14 days left                                                                                                                                                |
| a client says `x509: certificate signed by unknown authority`                                         | the client does not trust the CA: hand it `ca.crt` (generated) or the CA of your certificate                                                                          |
| a client says `x509: certificate is valid for ..., not ...`                                           | the name that the client uses is not in the certificate: `tls_names`, or the name of your certificate                                                                 |
| Docker: the certificate is not renewed                                                                | a single file was mounted, not the directory; or the check interval has not passed yet: send `SIGHUP`                                                                 |
| `http: TLS handshake error ... client sent an HTTP request`                                           | a client uses `http://` on the HTTPS port: use `https://`; there is no redirect, and the metrics port is a different port                                             |
| start: `allow_http and tls_cert exclude each other`                                                   | a listener that has TLS speaks nothing else: remove `allow_http`                                                                                                      |
| start: `address already in use` on the metrics port                                                   | the default is 9100, the port of the node exporter: if that runs on the same host, map the port at Docker (`-p 9101:9100`) or set `listen_metrics`                    |
| start (client): `the address ... is not loopback: it needs the list of the peers (allow_from_health)` | the health port is on a network address: name who may connect, or keep the default (loopback)                                                                         |
| Prometheus gets `403 Forbidden` from the metrics port                                                 | the address of Prometheus is not in `allow_from_metrics`: run with `-verbose` and read the refused address in the log                                                 |
| Prometheus gets `connection refused` on the metrics port                                              | it is on another host or container and `listen_metrics` is on loopback; or the metrics port was changed with `listen_metrics`                                         |

## What this does not do

- No client certificates (mTLS): who may connect is decided by `allow_from` and the token.
- No ACME of its own: let an ACME client (certbot, lego, Caddy, cert-manager) write the files; the program picks them up.
- No OCSP stapling, no SNI selection of several certificates: one pair per listener.
- The generated CA is for a try, not for production.

# eas-mock-server: try everything without Traveler

A stand-in ActiveSync server for demos, development and CI. [Back to the overview](../README.md).

Contents: [Try everything without Traveler (eas-mock-server)](#try-everything-without-traveler-eas-mock-server) | [What it speaks](#what-it-speaks) | [Simulated misbehavior for negative tests (`-sim`)](#simulated-misbehavior-for-negative-tests--sim)

## Try everything without Traveler (eas-mock-server)

```
# terminal 1: mock ActiveSync server with 50 ms latency and a new mail per device every 10 s
./eas-mock-server -listen 127.0.0.1:8099 -latency 50ms -mail-every 10s
# terminal 2: the nshtestherd container with 20 generated accounts (password secret)
docker run --rm -p 8788:8788 nshtestherd:latest -bind 0.0.0.0 -generate 20 -password secret
# terminal 3: the load worker, 5 users with 2 devices each
./eas-load -herd http://127.0.0.1:8788 -server http://127.0.0.1:8099 -clients 5 -devices 2 -profile iphone,ipad -metrics 127.0.0.1:9117
# terminal 4: start the load, watch it, stop it
curl -X POST -d 'target=all&command=run&job=eas-sync' --data-urlencode 'params=think=2-5&ping=30' http://127.0.0.1:8788/command
curl http://127.0.0.1:8788/status
curl http://127.0.0.1:9117/metrics
curl -X POST -d 'target=all&command=stop' http://127.0.0.1:8788/command

# the whole thing as an automatic test
./test.sh -e2e
```


## What it speaks

`eas-mock-server` lets you try the exporter and the load worker without a real Traveler. It is not
the real product; use it for demos, development and CI. The coordinator for the load tests is the real
[nshtestherd](https://github.com/nashcom/nshtestherd) (container `nshtestherd:latest`), or `eas-load`
runs standalone without one.

```
# mock ActiveSync server: any user name, one password, a tiny Inbox that gets new
# mail over time; latency and errors can be injected
./eas-mock-server -listen 127.0.0.1:8099 -password secret -mail-every 30s -latency 50ms -error-rate 0.01
```

`eas-mock-server` speaks plain HTTP on `/Microsoft-Server-ActiveSync` (OPTIONS,
Provision, Settings, FolderSync, Sync, Ping). `-hold-ping` (default on) holds a
Ping for its heartbeat and wakes it early with status 2 when `-mail-every`
brings new mail. `-drop-mail` accepts SendMail but never delivers the mail, to see how a lost mail shows up.
Because latency, error rate and mail rate are known, you can
check that eas-load and the exporter report exactly those numbers.

`tests/e2e.sh` starts the mock server and `eas-load` with 3 clients on loopback
ports, drives them with curl like the examples above and checks the numbers
(requests counted, mail items seen, pause, stop, state file) and runs a list of negative cases against the mock server with `-sim` (502, 403, 404, a wrong password, a closed connection, a timeout, EAS status errors and recoveries; each checks the exit code and the error class). It refuses to
start when one of its ports already answers.

```
./test.sh -e2e        # starts both programs and eas-load in the test container
```

## Simulated misbehavior for negative tests (`-sim`)

Started with `-sim`, `eas-mock-server` reads the request header `X-Mock-Sim` and misbehaves exactly as the
request asks, for that request only. The tools send the header with `-header` (`eas-probe`, `eas-load`), with
`headers` in an exporter target, or with `sim=...` in the parameters of an `eas-load` job, so a normal run
against the mock server can test a failure without restarting anything. Without `-sim` the header is ignored.

```
./eas-mock-server -listen 127.0.0.1:8099 -sim
./eas-probe -server http://127.0.0.1:8099 -user a@example.com -header "X-Mock-Sim: cmd=Sync,status=3,count=1"
```

The value is a list of `key=value` pairs, separated by commas or semicolons:

| Directive   | Effect                                                                                                        |
| ----------- | ------------------------------------------------------------------------------------------------------------- |
| `cmd=NAME`  | only for this command: `Sync`, `FolderSync`, `Ping`, `Provision`, `Settings`, `Search`, `SendMail`, `OPTIONS` |
| `count=N`   | only for the first N matching requests of a device; the next ones are answered normally (retry and recovery)  |
| `delay=3s`  | wait before handling the request (can be combined with the actions below)                                     |
| `http=502`  | answer with this HTTP status                                                                                  |
| `status=3`  | answer with this EAS status for the command (needs `cmd=`), for example `Sync` status 3                       |
| `garbage=1` | answer 200 with a body that is not WBXML                                                                      |
| `close=1`   | close the connection without an answer                                                                        |

For `eas-load`, give it as a job parameter, for example
`params=think=10-30&sim=cmd=Sync,http=502,count=3`: the header goes out with every request of that job and is
removed when the job ends. What happens before a request is parsed (connection refused, TLS errors, a server
that is down) is not a directive; test it with a closed port or a wrong host name.


# Performance test: eas-load, the coordinator nshtestherd, and the round trip script

The second deliverable of eas-tools. [Back to the overview](../README.md). It needs nshtestherd 0.9.2 or newer: from that version its options have one dash (`-bind`, `-generate`), like every option here.

Contents: [Round trip test (one command)](#round-trip-test-one-command) | [Load test (eas-load with nshtestherd)](#load-test-eas-load-with-nshtestherd) | [The load worker (eas-load)](#the-load-worker-eas-load) | [Standalone mode (no coordinator)](#standalone-mode-no-coordinator)

## Round trip test (one command)

`scripts/roundtrip.ps1` (Windows) and `scripts/roundtrip.sh` (Linux, macOS, Git Bash) run the whole
chain and check it: **first `eas-probe`** (all devices of the account; if it fails nothing else is started),
then a coordinator, `eas-load` with simulated users, and the jobs `eas-probe`, `eas-gal`, `eas-sync` and,
only if you ask for mail, `eas-mail` (mail to self, round trip), then stop. They print `ok` / `FAIL` per check and
exit with 0 (passed), 1 (a check failed) or 2 (usage or setup problem).

```
# demo: everything against the mock server and an nshtestherd container on this machine
.\scripts\roundtrip.ps1
scripts/roundtrip.sh

# a real server, your account only (small and gentle): the password from a file
.\scripts\roundtrip.ps1 -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -password-file d:\nsh_lab_pwd.txt
scripts/roundtrip.sh -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -password-file ~/.eas/pw

# the careful start with a real server: only the first step, the probe (read-only)
.\scripts\roundtrip.ps1 -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -password-file d:\nsh_lab_pwd.txt -only-probe
scripts/roundtrip.sh -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -password-file ~/.eas/pw -only-probe
```

| Option                              | Meaning                                                                                            |
| ----------------------------------- | -------------------------------------------------------------------------------------------------- |
| `-server`                           | real server (host or URL); without it the demo with the mock server runs                           |
| `-user`, `-password-file`           | the account (or `EASPROBE_PASSWORD`)                                                               |
| `-only-probe`                       | stop after the eas-probe step                                                                      |
| `-mail`, `-no-mail`                 | create test mail (round trip to yourself, deleted again): opt-in for a real server, on in the demo |
| `-herd`                             | use an existing coordinator (nshtestherd) that knows the account                                   |
| `-clients`, `-devices`              | simulated users and devices per user (real server: 1 and 1)                                        |
| `-probe-workers`                    | devices probed at the same time in the eas-probe step (default 1)                                  |
| `-mail-count`, `-mail-wait-seconds` | mails per device, seconds to wait for a mail to come back                                          |
| `-ping`                             | heartbeat seconds of the probe's held-Ping test (default 0, demo 5)                                |
| `-insecure`, `-ca-file`             | TLS options                                                                                        |
| `-bin`, `-verbose`                  | where the programs are; show the worker log at the end                                             |

The options have one dash, and `roundtrip.ps1` takes exactly the same names as `roundtrip.sh` (PowerShell does not care about upper and lower
case, and the older spellings `-PasswordFile`, `-OnlyProbe`, `-BinDir` still work). `-verbose` shows the worker log in both. `roundtrip.sh`
still reads a double dash as the same option, so nothing that calls it breaks.

**Mail is opt-in on a real server:** without `-mail` nothing is created in the mailbox
(the demo with the mock server has it on). With it, `eas-probe` sends one test mail per device to the account itself
and waits for it to come back, and `eas-load` runs `eas-mail`; the mails are deleted again (no copy in
Sent Items). The test devices stay in Traveler's device list.

Without `-Herd` the script starts the nshtestherd container (image `nshtestherd:latest` or `NSHTESTHERD_IMAGE`; Docker is needed).
With a real server it gets just your account: the password is written to a temporary file for the lifetime of the container and removed with it. `NSHTEST_TOKEN` is used when set.


## Load test (eas-load with nshtestherd)

```
# 1. the coordinator hands out the accounts (nshtestherd)
export NSHTEST_TOKEN=...                              # only if the coordinator has a token

# 2. one worker with 50 simulated users, one device each
./eas-load -herd http://coordinator:8788 -server traveler.lab.dnug.eu -clients 50

# 3. the same with 3 devices per user: iPhone, iPad, second iPhone
./eas-load -herd http://coordinator:8788 -server traveler.lab.dnug.eu -clients 50 \
          -devices 3 -profile iphone,ipad

# 4. several workers in parallel from one coordinator: different -name each
./eas-load -herd http://coordinator:8788 -server traveler.lab.dnug.eu -clients 50 -name w1 -metrics :9117
./eas-load -herd http://coordinator:8788 -server traveler.lab.dnug.eu -clients 50 -name w2 -metrics :9118

# a gentle start: a few clients first, slow ramp-up, summary every 5 s
./eas-load -herd http://coordinator:8788 -server traveler.lab.dnug.eu -clients 3 -ramp 1s -report 5s

# control the load through the coordinator
curl -X POST -d 'target=all&command=run&job=eas-probe' http://coordinator:8788/command
curl -X POST -d 'target=all&command=run&job=eas-sync' --data-urlencode 'params=think=10-30' http://coordinator:8788/command
curl -X POST -d 'target=all&command=run&job=eas-sync' --data-urlencode 'params=ping=60&duration=600' http://coordinator:8788/command
curl -X POST -d 'target=all&command=run&job=eas-gal' --data-urlencode 'params=mode=typeahead&think=10-30' http://coordinator:8788/command
curl -X POST -d 'target=all&command=run&job=eas-mail' --data-urlencode 'params=to=self&think=30-120&wait=60' http://coordinator:8788/command
curl -X POST -d 'target=all&command=pause&pause_seconds=30' http://coordinator:8788/command
curl -X POST -d 'target=all&command=idle' http://coordinator:8788/command
curl -X POST -d 'target=all&command=stop' http://coordinator:8788/command
curl http://coordinator:8788/status
curl http://localhost:9117/metrics                    # the worker's own numbers
```


## The load worker (eas-load)

`eas-load` is an [nshtestherd](https://github.com/nashcom/nshtestherd) worker
that generates ActiveSync load. One process hosts N simulated devices; each
registers with the coordinator as its own client (own `test_id`, own account,
own device id, own HTTP connection and cookie jar) and runs the jobs the
coordinator asks for. Start several processes (different `-name`) to get more
sessions in parallel from the same coordinator.

```
export NSHTEST_TOKEN=...                      # only if the coordinator has a token
./eas-load -herd http://coordinator:8788 -server traveler.example.com -clients 50
```

`-devices N` runs N devices per account at the same time, like a user with a phone
and a tablet: every device is its own session with its own device id and its own
entry in Traveler's device list, and a job runs on all of them. `-profile`
takes a comma separated list that is used in turn, so `-devices 3 -profile
iphone,ipad` gives an iPhone, an iPad and a second iPhone. The first device of an
account keeps its registration when you add more devices later. If one device
of an account fails a job, the job fails for all of them: they share the login.

The coordinator hands out the accounts and controls the load. The login is the
account's internet address (`-login shortname` to change). The default device
profile is `iphone`; the device ids are stable, so a restarted worker uses the
same registered devices (state in `<user config dir>/easprobe/load-<name>.json`,
saved every 30 s and on exit).

```
# one full flow per device (options, provision, folders, sync), result ok/failed
curl -X POST -d 'target=all&command=run&job=eas-probe' http://coordinator:8788/command

# keep syncing like a phone: think time 10-30 s between syncs
curl -X POST -d 'target=all&command=run&job=eas-sync' \
     --data-urlencode 'params=think=10-30' http://coordinator:8788/command

# mail: every device sends mail to itself through ActiveSync and measures how long it takes
# to come back, then deletes it and keeps no copy in Sent Items
curl -X POST -d 'target=all&command=run&job=eas-mail' \
     --data-urlencode 'params=to=self&think=30-120&size=1-10&wait=60&cleanup=yes' http://coordinator:8788/command

# the same to other test users: picked from the address book, nothing is tracked
curl -X POST -d 'target=all&command=run&job=eas-mail' \
     --data-urlencode 'params=to=others&think=30-120&cleanup=no' http://coordinator:8788/command

# directory lookups: every device looks for its own user in the address book
curl -X POST -d 'target=all&command=run&job=eas-gal' \
     --data-urlencode 'params=think=10-30' http://coordinator:8788/command

# the same like a user typing a name: the growing prefixes, then the full address
curl -X POST -d 'target=all&command=run&job=eas-gal' \
     --data-urlencode 'params=mode=typeahead&think=10-30' http://coordinator:8788/command

# push mode: hold a Ping of 60 s, sync when the server reports changes; end after 10 minutes
curl -X POST -d 'target=all&command=run&job=eas-sync' \
     --data-urlencode 'params=ping=60&duration=600' http://coordinator:8788/command

# pause everyone for 30 s, then stop
curl -X POST -d 'target=all&command=pause&pause_seconds=30' http://coordinator:8788/command
curl -X POST -d 'target=all&command=stop' http://coordinator:8788/command
```

| Job         | Parameters                                                                                                                                                                                                                                                                                                                                                                                                                                                       | Result                                                                                                         |
| ----------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------- |
| `eas-probe` | none                                                                                                                                                                                                                                                                                                                                                                                                                                                             | `ok` or `failed` after one full flow                                                                           |
| `eas-gal`   | `mode=self` or `typeahead`, `query=TEXT`, `think=MIN-MAX` seconds, `duration=N` seconds                                                                                                                                                                                                                                                                                                                                                                          | `stopped` when a newer command ends it, `ok` after `duration`                                                  |
| `eas-mail`  | `to=self` or `others` or an address list, `think=MIN-MAX`, `size=MIN-MAX` (KB of text), `attach=MIN-MAX` (KB of each attachment, default none), `attach_type=txt` (default, readable text files) or `bin`, `save_sent=yes` or `no` (a copy in Sent Items, default no), `files=N` (attachments per mail, default 1; text and attachments together at most 10240 KB), `wait=N`, `arrival=poll` (default) or `push`, `cleanup=yes` or `no`, `count=N`, `duration=N` | `ok` after `count`/`duration`, `stopped` by a newer command, `failed` on a refused login or 10 errors in a row |
| `eas-sync`  | `think=MIN-MAX` seconds (default 10-30), `ping=N` heartbeat seconds (push), `duration=N` seconds                                                                                                                                                                                                                                                                                                                                                                 | `stopped` when a newer command ends it, `ok` after `duration`, `failed` after 10 errors in a row               |

`eas-mail` sends tagged mail (`[easload <worker>-<n> d<device> #<number>]`). To the account itself it
waits for the mail to show up in the Inbox and records the round trip (`eas_load_mail_arrival_seconds`). With
`cleanup=yes` (the default) the mail is deleted again.

A mail to self that has not come back within `wait` is not given up at once: the device keeps looking for as long
again. A mail that shows up then is **late** (`eas_load_mail_late_total`, `mail late` in the log) and is deleted like any
other; one that is still missing after both waits is **lost** (`eas_load_mail_lost_total`, `mail lost`). Both count as
failed steps (`mail_late`, `mail_lost`), but they mean different things: slow or never delivered. Each late or lost mail
is logged with a timeline for the first 10 (SendMail, the first Syncs, every Sync that brought changes with the
subjects, a note for every 15th quiet one) and the result file (`-result-file`) lists them all under `mail.not_on_time`. A
The timeline and the result also say when the server says it received the mail (`DateReceived`, `server_received_after_seconds`), which separates a mail that reached the mailbox late from one that the device saw late; the two clocks are not synchronised, so a difference of a few seconds means little. A mail that is not on time holds up its device for twice the `wait`.

By default no copy is kept in Sent Items (`save_sent=yes` asks for one, whatever `cleanup` says). Some servers keep a
copy of every sent mail anyway, although `SaveInSentItems` was left out of the SendMail (the lab Traveler does). Unless
the job has `save_sent=yes`, each device deletes these copies at the end of the job: it syncs Sent Items and deletes
the mails whose subject it sent in this run, exactly, and nothing else. The summary line and the result file say how
many were deleted (`sent_copies_deleted`, `sent_copies_failed`).

Mail to other users stays in their mailboxes: they are picked from the address book (accounts whose name looks like
the own one, without the digits) and cannot be cleaned up from here, so use `to=self` unless you want mail in other
mailboxes.

A refused login (401/403) ends an `eas-sync` job at once as `failed`; it is never
retried in a loop. Unknown jobs or parameters fail the job, never the worker. The worker follows
the coordinator's contract: every `command_id` is applied once, a job that ends
is reported with its result and the client goes back to `idle`, `stop` reports
`stopping` then `done`, and a coordinator restart (unknown `test_id`) ends the
client.

Numbers: a summary line every 10 s (`-report`), and `-metrics :9117` serves
`/metrics` (`eas_load_requests_total{op,result}`, `eas_load_request_duration_seconds`
histogram per command, `eas_load_http_status_total`, `eas_load_clients{state}`,
`eas_load_jobs_total`, `eas_load_items_total`, `eas_load_failures_total{class}`).
Each client needs a few sockets: raise the file descriptor limit (`ulimit -n`)
for large `-clients`.

The worker is tested against the mock server and the nshtestherd container; what has run against a
real Traveler is listed under [Validation status](DEVELOPMENT.md#validation-status).

## Standalone mode (no coordinator)

`eas-load` also runs on its own: the accounts and the job come from the command line, and the same jobs,
statistics and `/metrics` apply. It ends by itself and its exit code tells how it went.

```
# one user; -devices gives several devices of the same user (the password from a file or EASLOAD_PASSWORD)
./eas-load -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -password-file pw.txt -devices 2 \
           -job eas-mail -params 'to=self&think=30-120&count=10'

# many users from a CSV file in the nshtestherd format, stopped after 10 minutes
./eas-load -server traveler.lab.dnug.eu -users users.csv -job eas-sync -params 'think=10-30' -duration 10m
```

| Option                    | Meaning                                                                                         |
| ------------------------- | ----------------------------------------------------------------------------------------------- |
| `-user`, `-password-file` | one account; the password is read from the first line of the file or from `EASLOAD_PASSWORD`    |
| `-users FILE`             | CSV with the header `FirstName,LastName,Password,Shortname,InternetAddress`; quoted fields work |
| `-clients N`              | with `-users`: use the first N accounts (default: all); one `-user` is always one client        |
| `-job`, `-params`         | the job and its parameters, exactly as in a coordinator command                                 |
| `-duration D`             | stop the job after this time (a stop by duration is a success)                                  |
| `-result-file FILE`       | write the outcome and the numbers per command as a JSON file                                    |

The job starts when every client is registered. Exit code 0: every job ended ok or was stopped by `-duration`;
1: a job failed (a refused login, errors in a row); 2: wrong options. `-herd` and `-user`/`-users` exclude each
other: the worker is either in coordinator mode or standalone. For one user with several clients use `-devices`:
every client needs its own account, because the device ids come from user, server and device number.

Instead of a long command line, the options can come from environment variables and from a JSON file. Every option is
also `EASLOAD_<OPTION>` (upper case, `-` written as `_`: `EASLOAD_PASSWORD_FILE`, `EASLOAD_DEVICES`, `EASLOAD_PARAMS`) and
a key of the file given with `-config` or `EASLOAD_CONFIG` (`password_file`; `header` takes a list). The command line wins
over the environment, the environment over the file, the file over the default. A variable with the prefix that matches
no option is reported as a warning, and an unknown key in the file is an error. Examples: `deploy/load.example.env` (for
`docker run --env-file`), `deploy/load-standalone.example.json` and `deploy/load-coordinator.example.json`.

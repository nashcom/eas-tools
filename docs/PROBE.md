# eas-probe: one ActiveSync probe on the command line

The probe is the engine of the exporter and a tool of its own. [Back to the overview](../README.md).

Contents: [Quick start](#quick-start) | [Command line examples](#command-line-examples) | [Device identity](#device-identity) | [Steps](#steps) | [Output](#output) | [Options (most used)](#options-most-used)

## Quick start

```
export EASPROBE_PASSWORD='...'
./eas-probe -server traveler.example.com -user alice
```

or let it find the server:

```
./eas-probe -email alice@example.com
```

Everything else is detected automatically:

| Detected           | How                                                                                |
| ------------------ | ---------------------------------------------------------------------------------- |
| EAS endpoint       | Autodiscover (when no `-server`); a bare host gets `/Microsoft-Server-ActiveSync`  |
| Protocol version   | HTTP `OPTIONS`, highest supported of 14.1, 14.0, 16.1, 16.0, 12.1, 12.0            |
| Server software    | `Server` / `MS-Server-ActiveSync` headers (Domino/Traveler, Z-Push, Exchange, ...) |
| Policy requirement | Provision; "no policy defined" is reported, not treated as an error                |
| Inbox              | `FolderSync`, remembered in the state file for later incremental runs              |
| Directory          | `Search` (GAL) lookup of the account's own address, when the server offers it      |
| Heartbeat limits   | `Ping` status 3/4 makes the probe retry once with the server's interval            |
| Lost registration  | HTTP 449 triggers a re-provision automatically                                     |


## Command line examples

All examples use the lab: server `traveler.lab.dnug.eu`, user `nsh@lab.dnug.eu`.
The password always comes from a file, the environment or stdin, never from
an option. Windows examples use `.\eas-probe.exe` (from `bin\win-amd64`), on Linux use `./eas-probe`
(or the binaries built by `./compile.sh` in `bin/<platform>/`, for example `./bin/linux-amd64/eas-probe`).

### One probe (eas-probe)

```
# Windows: password from a file (first line)
.\eas-probe.exe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -password-file d:\nsh_lab_pwd.txt

# Linux: password from the environment
export EASPROBE_PASSWORD='the-password'
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu

# password from stdin (a password manager)
pass show lab/traveler | ./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -password-stdin

# every request logged, including the successful ones
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -debug

# test a held Ping: keep a Ping open for 60 seconds (use 120 or more to test the proxy timeouts)
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -ping 60

# machine readable: JSON, or Prometheus text for a textfile collector
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -json
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -prom

# the directory (address book) lookup of the own user is part of the probe
# when the server offers it; look for something else, or switch it off
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -directory-query "Daniel Nashed"
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -directory off

# several devices of the account, one after the other (each its own device in Traveler)
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -devices 3 -profile iphone,ipad

# the same, up to 3 devices at the same time (the first device runs alone first, as the login pilot:
# a wrong password costs one failed login, not three)
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -devices 6 -profile iphone,ipad -workers 3

# mail: send a mail to the account itself, wait for it to come back (held Ping), delete it again.
# Creates a mail, so it is opt-in; the round trip time is the result.
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -mail
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -mail -mail-wait-seconds 120 -mail-keep

# a private CA, or (only for a test) no certificate check
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -ca-file myca.pem
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -insecure

# register the device again (same device id), or never provision
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -fresh
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -provision never
```

Several devices of one user, by hand (each profile is its own device in Traveler,
`-device-id` makes another one of the same profile):

```
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -profile iphone
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -profile ipad
./eas-probe -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -profile iphone -device-id 11111111111111111111111111111111
```

### Settings: command line, environment, file

All three programs take their settings the same way, and an option has the same name everywhere. The order of
precedence is: command line, then environment variable, then JSON file, then the default.

| Program                 | Environment variable prefix | Config file                      | Command line options                                            |
| ----------------------- | --------------------------- | -------------------------------- | --------------------------------------------------------------- |
| `eas-probe`             | `EASPROBE_`                 | `-config` / `EASPROBE_CONFIG`    | all                                                             |
| `eas-load`              | `EASLOAD_`                  | `-config` / `EASLOAD_CONFIG`     | all                                                             |
| `eas-blackbox-exporter` | `EASEXPORTER_`              | `-config` / `EASEXPORTER_CONFIG` | `-config`, `-verbose`, `-debug`, `-log-format`, `-health-check` |

The name of an option is written with `-` on the command line (`-password-file`) and with `_` in the file
(`password_file`) and in the variable (`EASLOAD_PASSWORD_FILE`: the prefix and the name in upper case). Where the
programs share a setting, it has the same name in all of them: `server`, `email`, `user`, `profile`, `device-id`,
`password-file`, `password-env`, `provision`, `directory`, `directory-query`, `mail`, `mail-wait-seconds`, `insecure`,
`ca-file`, `state-file`, `no-state`. The simplest way to give the password is in the JSON file itself (the key `password`),
so there is no other file to manage; the variable `<PREFIX>PASSWORD` (`EASPROBE_PASSWORD`, `EASLOAD_PASSWORD`,
`EASEXPORTER_PASSWORD`) is the same key. Stricter setups can keep the password in a separate file (`password-file`, for
example a Docker secret) or name another variable that holds it (`password-env`, where each target of the exporter can
name its own). If more than one is given, the file wins, then the password, then the variable name. The password
itself is never a command line option, because everybody on the machine can read the command lines of running programs; a
JSON file with a password has to be readable only by its owner (`chmod 600`) and must not be committed. A variable with
the prefix that matches no option is reported as a warning, which catches typos, and an unknown key in a file is an
error. Examples are in `deploy/`: `*.example.json`, `*.example.env` and the compose files.

**Log level.** The levels have the names and the order of OpenTelemetry, from the quietest: `warn`, `info`, `debug`,
`trace`. A level shows itself and everything more severe. `warn` shows problems only. **`info` is the default**: the
normal operational events, such as the start, a configuration that was loaded, changed or could not be fetched and
works again, an entry of the mail monitor that was added, changed or removed, the progress and result of an `eas-load`
run, and the result of every mail of the mail monitor (`mail monitor: answered`, with the round trip). `debug` adds the routine
activity (a line for every probe, every mail sent), and `trace` the very
detailed output (why a configuration check found nothing new, every HTTP request, the library output). `warn`, `info`
and `debug` are the levels of Go's `slog`; `trace` is one below `debug`, written `TRACE`, as in OpenTelemetry. The
setting is `log_level` in the JSON file and `EASPROBE_LOG_LEVEL`, `EASLOAD_LOG_LEVEL` or `EASEXPORTER_LOG_LEVEL` in the
environment. On the command line `-verbose` selects `debug` and `-debug` selects `trace`, and they win over the file
and the environment (the more detailed one wins when both are given). The exporter applies a changed `log_level` of a
reloaded configuration at once.

**Log format.** `log_format` is `text` (the default) or `json`: in the JSON file as `log_format`, as `EASPROBE_LOG_FORMAT`,
`EASLOAD_LOG_FORMAT` or `EASEXPORTER_LOG_FORMAT`, and on the command line as `-log-format json`. The text format has the
time in UTC, the level and the message first, with no `time=`, `level=` or `msg=` in front, and then the attributes as
`key=value`:

```
2026-10-10T15:17:14.251Z INFO  configuration reloaded source=https://nashcom.de/exporter-dnuglab.json targets=[traveler-lab]
```

The json format is one object per line in the same shape as the log records of the Domino tools, so one query and one
dashboard in Loki or VictoriaLogs serve both. The field names are those of OpenTelemetry's log data model (as in its
protobuf definition): `time_unix_nano` and `observed_time_unix_nano` (Unix nanoseconds in a string; they are the same,
because a message is written when it happens), `severity_number` (TRACE 1, DEBUG 5, INFO 9, WARN 13, ERROR 17),
`severity_text` and `body` (the message). `scope` names the library (`easprobe`) and its version. `resource` describes
the program: `service.name`, `service.version`, `service.instance.id` and `host.name` (the host name), `os.type`,
`process.executable.name` and `process.pid`. `attributes` holds the other fields of the message, empty when there are none:

```
{"time_unix_nano":"1791650970799591200","observed_time_unix_nano":"1791650970799591200","severity_number":9,"severity_text":"INFO","body":"configuration reloaded","scope":{"name":"easprobe","version":"0.9.0"},"resource":{"service.name":"eas-blackbox-exporter","service.version":"0.9.0","service.instance.id":"traveler-monitor","host.name":"traveler-monitor","os.type":"linux","process.executable.name":"eas-blackbox-exporter","process.pid":1},"attributes":{"source":"https://nashcom.de/exporter-dnuglab.json","targets":["traveler-lab"]}}
```

A collector or a log forwarder can read that line almost as it is. The exporter applies a changed `log_format` of a reloaded
configuration at once.


## Device identity

The default profile imitates an iPhone: DeviceType `iPhone`, User-Agent
`Apple-iPhone13C3-easprobe/2307.90`, plus a `settings` step that sends the
device information a phone sends after Provision. Traveler shows the device
as `Apple-iPhone13C3-easprobe` (Device Name = User-Agent before the `/`), so
it is recognized as an Apple device but is easy to tell from real phones.
`-profile easprobe` uses a plainly synthetic identity (`easprobe/<version>`)
instead. Each profile is its own device in Traveler.

## Steps

`autodiscover` (only without `-server`) -> `options` -> `provision` ->
`settings` (iOS profiles; a failure is reported but does not stop the run) ->
`foldersync` -> `sync` (Inbox) -> `directory` (look for the own user in the
address book, when the server offers `Search`; a failure is reported but does not
stop the run) -> `ping` (only with `-ping N`).
A failed step marks the following steps as skipped.

The first run registers the device (Provision) and does the initial sync;
later runs reuse the stored policy key and sync keys, so they exercise the
incremental path. Use `-fresh` to register again with the same device id.

## Output

* text (default), `-json`, or `-prom` (Prometheus text format, usable as a
  node_exporter textfile or from a later `/probe` handler)
* exit code: 0 all steps ok, 1 a step failed, 2 usage/config error
* every failing step carries an error class: `auth`, `forbidden`,
  `not_found`, `provision_required`, `server_error`, `eas_status`, `tls`,
  `dns`, `network`, `timeout`

## Options (most used)

| Option                                                       | Meaning                                                                                                                 |
| ------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------- |
| `-server`, `-email`, `-user`                                 | endpoint, autodiscover address, login                                                                                   |
| `-password-env` / `-password-stdin` / `-password-file`       | password source (never a plain flag)                                                                                    |
| `-ping N`                                                    | add a Ping holding up to N seconds                                                                                      |
| `-devices N`, `-profile a,b`                                 | probe N devices of the account, profiles in turn                                                                        |
| `-workers N`                                                 | probe up to N devices at the same time (pilot first)                                                                    |
| `-mail`, `-mail-wait-seconds`, `-mail-arrival`, `-mail-keep` | round trip mail to self (opt-in, only that mail is deleted again); arrival `poll` (default) or `push` (experiment)      |
| `-trace FILE`                                                | write every request and response, WBXML decoded, to FILE (no password, no cookies)                                      |
| `-header "Name: value"`                                      | extra request header on every request, repeatable (for example `X-Mock-Sim` for the mock server; `eas-load` has it too) |
| `-directory auto\                                            | on\                                                                                                                     |
| `-provision auto\                                            | always\                                                                                                                 |
| `-fresh`, `-no-state`, `-state-file FILE`                    | state handling                                                                                                          |
| `-profile iphone\                                            | ipad\                                                                                                                   |
| `-device-id`, `-device-type`, `-user-agent`                  | override single identity values                                                                                         |
| `-insecure`, `-ca-file FILE`                                 | TLS verification                                                                                                        |
| `-verbose`, `-debug`                                         | log at level debug (the routine activity) or trace (the details) on stderr; the default is info (`log_level`)           |
| `-log-format FORMAT`                                         | format of the log (`log_format`): text (the default) or one JSON object per line in the field names of OpenTelemetry    |

The device id is derived from user and server, so repeated probes are one
device in Traveler. State of all devices lives in memory and in one file,
`<user config dir>/easprobe/state.json`, loaded at startup and saved on exit
(long-running users call `Store.AutoSave`). No database is needed.
The file has mode 0600 and holds policy keys and sync keys, not passwords.


# eas-client: an ActiveSync mail client for the command line and REST

`eas-client` reads and sends mail through ActiveSync. It is a program of its own, built on the same packages as the probe,
the load worker and the exporter (`probe` for the protocol, `client` for the operations). The command line and the REST API
call the same functions and print **the same JSON**: what `eas-client list inbox` prints is, byte for byte, the answer of
`GET /accounts/NAME/folders/inbox/messages`.

Contents: [Quick start](#quick-start) | [Commands](#commands) | [Output](#output) | [REST API](#rest-api) | [Accounts and settings](#accounts-and-settings) | [In a container](#in-a-container) | [The device and its state](#the-device-and-its-state) | [What to know](#what-to-know) | [Try it without Traveler](#try-it-without-traveler)

## Quick start

```
export EASCLIENT_PASSWORD='...'

eas-client -server traveler.example.com -email alice@example.com folders
eas-client -server traveler.example.com -email alice@example.com list inbox
eas-client -server traveler.example.com -email alice@example.com get inbox 5:12
```

Options go before the command. Reading is always allowed. Changing the mailbox (read, unread, move, delete) needs `-write`,
and sending a mail needs `-send`. They are two switches: a mail leaves the mailbox and cannot be taken back, so `-write`
does not include it, and `-send` does not allow deleting.

```
eas-client ... -send send -to bob@example.com -subject "Hello" -body "the text"
eas-client ... -write read inbox 5:12
eas-client ... -write move inbox 5:12 archive
eas-client ... -write delete inbox 5:12
```

## Commands

| Command                                                                                                                        | What it does                                                                                                                 |
| ------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------- |
| `accounts`                                                                                                                     | the accounts of the configuration (never the passwords)                                                                      |
| `info`                                                                                                                         | server product, protocol version, commands, device id, number of folders                                                     |
| `folders`                                                                                                                      | the folders: id, name, type                                                                                                  |
| `list FOLDER [-limit N\|all] [-preview BYTES] [-since 2w]`                                                                     | the mails of the date window, newest first (default 20, no body)                                                             |
| `get [FOLDER] ID`                                                                                                              | one mail with its whole body                                                                                                 |
| `calendar list [FOLDER] [-from D] [-to D] [-since 2w] [-limit N\|all]`                                                         | the events of the calendar by start time; from now on by default                                                             |
| `calendar get [FOLDER] ID`                                                                                                     | one event with all details                                                                                                   |
| `calendar create [FOLDER] -subject S -start T [-end T or -duration D] [-all-day] [-location L] [-reminder N] [-attendee A]...` | create an event (`-write`); with `-attendee`, `-optional` or `-resource` it is a meeting that invites them (`-send` as well) |
| `calendar delete [FOLDER] ID`                                                                                                  | delete an event (`-write`; a meeting with attendees needs `-send` too)                                                       |
| `calendar ics [FOLDER] ID`                                                                                                     | the event as an iCalendar file (`.ics`): the raw text on stdout, not JSON (`eas-client calendar ics 8:123 > event.ics`)      |
| `contacts list [FOLDER] [-search T] [-limit N or all]`                                                                         | the contacts of the address book by name (JSON, no photo)                                                                    |
| `contacts get [FOLDER] ID`                                                                                                     | one contact with all details (JSON)                                                                                          |
| `contacts vcf [FOLDER] ID`                                                                                                     | one contact as a vCard file (`.vcf`), with the photo: raw text on stdout, not JSON                                           |
| `contacts export [FOLDER] [-search T]`                                                                                         | all contacts (or the search result) as one vCard file: `eas-client contacts export > contacts.vcf`                           |
| `eml [FOLDER] ID`                                                                                                              | the whole message as a mail file (`.eml`): the raw bytes on stdout, not JSON (`eas-client eml 4:10207 > mail.eml`)           |
| `send -to A [-cc B] -subject S -body T`                                                                                        | send a mail; the text from `-body`, `-body-file FILE` or `-body-stdin`                                                       |
| `reply [FOLDER] ID [-all] -body T`                                                                                             | reply to a mail; the server adds the original (SmartReply); `-all` also to the other recipients                              |
| `forward [FOLDER] ID -to A [-cc B] -body T`                                                                                    | forward a mail; the server adds the original (SmartForward)                                                                  |
| `read [FOLDER] ID`, `unread [FOLDER] ID`                                                                                       | set the read flag                                                                                                            |
| `move [FOLDER] ID DESTINATION`                                                                                                 | move a mail; the answer has the new id                                                                                       |
| `delete [FOLDER] ID`                                                                                                           | delete a mail: it goes to Deleted Items, from there it is gone for good                                                      |
| `serve`                                                                                                                        | the REST API                                                                                                                 |

`FOLDER` is a folder id, or `inbox`, `sent`, `drafts`, `deleted`, `outbox`, `calendar`, `contacts`, `tasks`, `notes`, or the
name of the folder (case does not matter; a name two folders share has to be given as id). `send` keeps a copy in Sent Items
unless `-no-sent-copy` is given.

## Output

Success: JSON on stdout, exit code 0. Failure: a JSON error object on **stderr**, exit code 1; the REST API answers with the
same object and the HTTP status. A command that was written wrong (unknown option, missing argument) prints a text and exits
with 2.

```
$ eas-client ... list inbox -limit 1
{
  "account": "default",
  "folder": { "id": "5", "name": "Inbox", "type": "inbox" },
  "count": 1,
  "messages": [
    {
      "id": "5:12",
      "folder": "5",
      "subject": "Hello",
      "from": "Bob <bob@example.com>",
      "date": { "time": "2026-10-10T08:15:03Z", "unix_nano": "1791620103000000000" },
      "read": false, "flagged": false, "importance": "normal", "has_attachments": false,
      "body_size": 1423, "body_truncated": false
    }
  ]
}

$ eas-client ... list nothing
{ "error": { "code": "folder_not_found", "message": "no folder \"nothing\"" } }
```

Times have the two forms of the other tools: readable UTC and Unix nanoseconds. Error codes: `account_required`,
`unknown_account`, `folder_required`, `folder_not_found`, `folder_ambiguous`, `id_required`, `mail_not_found`,
`recipient_required`, `subject_required`, `no_sender`, `write_disabled`, `send_disabled`, `auth_failed` (the server refused the login), `server_error`,
`eas_status`, `timeout`, `unauthorized` (REST token), `bad_request`, `not_found`, `method_not_allowed`.

## REST API

```
eas-client -config client.json -auth-token-file token -listen 127.0.0.1:8080 serve   # plain HTTP, on loopback only
curl -H "Authorization: Bearer $(cat token)" http://127.0.0.1:8080/accounts/lab/folders/inbox/messages?limit=5
```

| Request                                                           | Same as the command                                                                                                                                                                              |
| ----------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `GET /health`                                                     | (no token needed)                                                                                                                                                                                |
| `GET /accounts`                                                   | `accounts`                                                                                                                                                                                       |
| `GET /accounts/{account}`                                         | `info`                                                                                                                                                                                           |
| `GET /accounts/{account}/folders`                                 | `folders`                                                                                                                                                                                        |
| `GET /accounts/{account}/folders/{folder}/messages`               | `list` (`?limit=N\|all`, `?preview=BYTES`)                                                                                                                                                       |
| `GET /accounts/{account}/folders/{folder}/messages/{id}`          | `get`                                                                                                                                                                                            |
| `POST /accounts/{account}/messages`                               | `send` (body: `{"to":[...],"cc":[...],"subject":"...","body":"...","save_in_sent":true}`)                                                                                                        |
| `PATCH /accounts/{account}/folders/{folder}/messages/{id}`        | `read`, `unread`, `move` (body: `{"read":true}` or `{"move_to":"archive"}`)                                                                                                                      |
| `DELETE /accounts/{account}/folders/{folder}/messages/{id}`       | `delete`                                                                                                                                                                                         |
| `GET /accounts/{account}/folders/{folder}/messages/{id}/eml`      | `eml`: the raw message, `Content-Type: message/rfc822`, not JSON (an error is the JSON error)                                                                                                    |
| `GET /accounts/{account}/calendar/events`                         | `calendar list` (`?from=`, `?to=`, `?since=`, `?limit=N\|all`)                                                                                                                                   |
| `GET /accounts/{account}/contacts`                                | `contacts list` (`?search=`, `?limit=N` or `all`; also under `/folders/{folder}/contacts`)                                                                                                       |
| `GET /accounts/{account}/contacts/{id}`                           | `contacts get`                                                                                                                                                                                   |
| `GET /accounts/{account}/contacts/{id}/vcf`                       | `contacts vcf`: `Content-Type: text/vcard`, not JSON                                                                                                                                             |
| `GET /accounts/{account}/contacts.vcf`                            | `contacts export` (`?search=`): all contacts as one vCard file                                                                                                                                   |
| `POST /accounts/{account}/calendar/events`                        | `calendar create` (body: `{"subject":"...","start":"2026-10-20T09:00:00Z","duration":"1h","attendees":[{"email":"a@b.c","name":"A","type":"required"}]}`; also under `/folders/{folder}/events`) |
| `DELETE /accounts/{account}/calendar/events/{id}`                 | `calendar delete`                                                                                                                                                                                |
| `GET /accounts/{account}/calendar/events/{id}/ics`                | `calendar ics`: `Content-Type: text/calendar`, not JSON (also under `/folders/{folder}/events/{id}/ics`)                                                                                         |
| `GET /accounts/{account}/calendar/events/{id}`                    | `calendar get` (the same under `/folders/{folder}/events` for another calendar folder)                                                                                                           |
| `POST /accounts/{account}/folders/{folder}/messages/{id}/reply`   | `reply` (body: `{"body":"...","all":false,"save_in_sent":true}`)                                                                                                                                 |
| `POST /accounts/{account}/folders/{folder}/messages/{id}/forward` | `forward` (body: `{"to":[...],"cc":[...],"body":"..."}`)                                                                                                                                         |

Path parts are URL-encoded (a mail id with a slash goes as `%2F`). The account is part of the path; unlike the command line,
the API does not pick "the only account" for you.

**Description of the API.** `GET /openapi.json` is an OpenAPI 3 description of every request: the paths and their parameters,
the JSON types of the requests and answers, the raw files, the error body, and which switch a request needs. It is made from the
table of the routes and from the Go types, so it follows the code, and a test checks that every route is described and served. With
`-docs` (config `docs`) `GET /docs` is a page (Swagger UI) where the requests can be tried: the Authorize button takes the token.
**Swagger UI is not part of the program or of its source.** The page makes the *browser* load it from an external location: by
default the CDN jsDelivr, from the stream of version 5 (`swagger-ui-dist@5`: the newest 5.x.y, so fixes arrive by themselves and a new
major version does not). A moving location cannot have integrity hashes, so the browser runs what the CDN serves under that name; the
log says `integrity_check=false`. For the hashes pin an exact version that the program knows (today 5.33.1): `-docs-ui
https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.33.1`, then the browser refuses a file that is not that version. The program serves only the page and a short script (`/docs/docs-init.js`); the content
security policy of the page allows scripts of the program and of that one place, and calls to the program only. The browser, not the
server, needs access to the CDN. For an air-gapped network or a copy of your own, give the place: `-docs-ui https://mirror.example/swagger-ui`
(config `docs_ui`, `EASCLIENT_DOCS_UI`), the URL of a directory with `swagger-ui.css` and `swagger-ui-bundle.js` from the npm package
`swagger-ui-dist`; then the page has no integrity hashes (the files are not known) and the policy names that place. A browser cannot send
a bearer token when it opens a page, so **with `-docs` the page, its script and `/openapi.json` need no token**: they say what the API is and hold no data. Every request of the API itself still needs
the token, and without `-docs` `/openapi.json` needs it too and `/docs` is not there. The page uses absolute paths (`/docs/...`,
`/openapi.json`): put the API at the root of its host, not under a path prefix of a proxy.

**Security.** The API gives access to whole mailboxes.

- It listens on **`:8443` (HTTPS)** by default, and on `127.0.0.1:8080` with `-allow-http`. On any address that is not loopback it
  **refuses to start without a token** (`-auth-token-file`, or `EASCLIENT_AUTH_TOKEN`), and a network address without TLS needs `-allow-http`
  as well: the token and the mail would cross the network in clear text otherwise.
- `-allow-from` (config `allow_from`, `EASCLIENT_ALLOW_FROM`) restricts the listener to a list of peers: addresses, networks
  (`192.168.0.0/16`), ranges (`10.0.0.5-10.0.0.20`) and the name `private`. It guards **every** endpoint, `/health`, `/docs` and
  `/openapi.json` included, before the token is looked at; a peer that is not in the list gets `403 Forbidden` and nothing else. The
  loopback interface is always allowed. The address that counts is the one of the connection: behind a proxy it is the proxy, and a
  proxy on the same host connects from loopback, so everything it passes on is allowed too. In Docker with a published port the peer
  can be the gateway of the bridge network (172.17.0.1) and not the real client: allow what you see in the log (`-verbose` shows
  every refused request).
- **TLS.** `-tls-cert` and `-tls-key` (config `tls_cert`, `tls_key`; `EASCLIENT_TLS_CERT`, `EASCLIENT_TLS_KEY`) serve HTTPS. The files are
  watched (`-tls-check-interval`, default one minute) and loaded again on `SIGHUP`: a renewed certificate is served without a restart, and
  a pair that does not fit or is expired is refused while the one in use stays (the log says so). Docker and Kubernetes secrets that
  are swapped by a link are seen. `-tls-generate` (`tls_generate`) makes a small CA and a certificate when the files are not there
  (`-tls-names` for its names; never overwrites; the CA is kept next to it as `ca.crt` and `ca.key`): for a try, not for production.
  **TLS is the default.** Off the loopback, with no certificate named and no `-allow-http`, the client looks for the certificate in
  `/run/secrets/tls` (`tls.crt`, `tls.key`, `ca.crt`, the same default and the same directory as the exporter, so a container can share
  one volume) and makes one there with its own small CA when there is none; a certificate that it made is made again at every start, one of
  yours is never touched ([TLS.md](TLS.md)). `-allow-http` (`allow_http`, `EASCLIENT_ALLOW_HTTP`) serves plain HTTP on purpose, for a proxy
  that terminates TLS, and `-allow-from` should name it. A loopback address (the default of `-allow-http`, `127.0.0.1:8080`) needs no choice and gets no
  certificate, so a local NGINX in front works as it is; on Kubernetes keep TLS behind the ingress. With TLS a second listener runs on the loopback interface and the port after `-listen`
  (`127.0.0.1:8444`, the port after `-listen`) that has **`/health` and nothing else** (the client has no metrics; the API, `/docs` and `/openapi.json` are on the
  HTTPS port only): it is there for the health check of a container. `-listen-health` (`listen_health`) names another address, which
  needs `-allow-from-health` (`allow_from_health`), the peers that may connect to it. Details and the exporter: [TLS.md](TLS.md).
- The token is only accepted as `Authorization: Bearer ...`, never in the URL, which ends up in logs.
- Changing the mailbox is off unless `-write` (or `"write": true` in the config) is given, and sending is off unless `-send`
  (`"send": true`) is given. A refused request is `403 write_disabled` or `403 send_disabled` and never reaches the server.
  `GET /accounts` shows both switches (`allow_write`, `allow_send`).
- Passwords are never part of an answer or of the log.

## Accounts and settings

A single account goes on the command line (`-server`, `-email`, `-user`, and the password from `EASCLIENT_PASSWORD`,
`-password-env NAME`, `-password-file FILE` or `-password-stdin`). It is called `default`.

Several accounts, and every other option, go in a JSON config file (`-config FILE` or `EASCLIENT_CONFIG`). `accounts` is a
list; each entry has a `name` and the keys of a target of the exporter:

```json
{
  "write": false,
  "send": false,
  "listen": "127.0.0.1:8080",
  "auth_token_file": "/run/secrets/eas-client-token",
  "accounts": [
    { "name": "lab",  "server": "traveler.example.com", "email": "alice@example.com", "password_file": "/run/secrets/alice" },
    { "name": "test", "server": "traveler.example.com", "email": "bob@example.com",   "password_env": "BOB_PASSWORD" }
  ]
}
```

The order of precedence is the one of the other tools: command line, then environment (`EASCLIENT_<OPTION>`), then the file,
then the default. With one account `-account` is not needed, with several it names the account. Accounts on the command line
and `accounts` in the file together are an error.

## In a container

The image of the exporter (`./build.sh`) holds `eas-client` too. [docker-compose.client.yml](../docker-compose.client.yml) runs it as
the REST API with the page of the documentation, and [deploy/eas-client-container.example.json](../deploy/eas-client-container.example.json)
is its configuration:

```
cp deploy/eas-client-container.example.json client.json     # the account, the token, the time zone
chmod 600 client.json
./build.sh
docker compose -f docker-compose.client.yml up -d
# the page: https://127.0.0.1:8443/docs  (the certificate is the micro CA's: the browser warns until it trusts ca.crt; Authorize takes the token of client.json)
```

In the container the API listens on `:8443` (HTTPS, the default) and so it needs a token; the port is published on the loopback interface of the host only. The certificate is in `/run/secrets/tls`, the volume `eas-tls`: the exporter of `docker-compose.yml` uses the same one.
A container is UTC, so give `timezone`. `state_file` is in the volume `/data`. The client reads only until `write` and `send` are
true. A single command can be run from the same image: `docker run --rm -e EASCLIENT_PASSWORD -v ./client.json:/etc/eas-tools/client.json:ro
--entrypoint /usr/local/bin/eas-client nashcom/eas-tools -config /etc/eas-tools/client.json -account lab folders`.

Without a file, everything can be an environment variable (`EASCLIENT_` and the name of the option in capitals):

```
docker run -d --name eas-client -p 127.0.0.1:8443:8443 \
  -e EASCLIENT_AUTH_TOKEN=change-me-to-a-long-random-text -e EASCLIENT_DOCS=true \
  -e EASCLIENT_SERVER=traveler.example.com -e EASCLIENT_EMAIL=alice@example.com -e EASCLIENT_PASSWORD='the-password' \
  -e EASCLIENT_TIMEZONE=Europe/Berlin -e EASCLIENT_STATE_FILE=/data/client-state.json \
  -v eas-client-data:/data -v eas-tls:/run/secrets/tls \
  --entrypoint /usr/local/bin/eas-client nashcom/eas-tools serve
```

The password and the token are then visible in `docker inspect`; for more than a test use the file or Docker secrets (`password_file`,
`auth_token_file`).

## The device and its state

The client is a device of its own (`#client` is part of the device id), so it does not share sync keys with the probe, the
load worker or the mail monitor. Its state (policy key, sync keys, folders) is kept in `client-state.json` next to the state
of the probe (`-state-file` to change, `-no-state` for none). The password is never stored.

## What to know

- **EAS has no listing.** `list` reads the folder from the start (a Sync from key 0), which the server answers with the mails
  of a date window. The window is `-since` (`1d`, `3d`, `1w`, `2w`, `1m`, `3m`, `6m`; default `2w`; the library cannot ask for
  more than 6 months), and the answer says which one it used (`"since"`). **A folder whose mail is older than the window
  lists as empty**: try `-since 6m`. `-limit` cuts after the server has answered.
- **Time zone.** Times without an offset (`-start 2026-10-20T09:00`, `-from 2026-10-01`) are times in the zone of the account:
  `-timezone Europe/Berlin` (config `timezone`, `EASCLIENT_TIMEZONE`; `time_zone` in an account), and without it the zone of the
  machine (the variable `TZ`, `/etc/timezone` or `/etc/localtime` on Linux and macOS, `tzutil /g` on Windows; a machine where none
  gives a zone, a container for example, is UTC). A new entry carries the zone with its rules of daylight saving time (the
  `TimeZone` of ActiveSync), so Notes does not say that it was "created in a different time zone", and an invitation has the zone
  block `VTIMEZONE` with the Windows name of the zone (`W. Europe Standard Time`, as Notes writes it) and local times
  (`DTSTART;TZID=W. Europe Standard Time:...`). On the wire and in the JSON that the commands print the times stay UTC; `info` says
  which zone is used (`time_zone`). A zone has to be a name of the zone database; the Windows names that Notes uses are known for
  the usual zones, and a zone without one is written with its own name.
- **Creating events.** `calendar create` makes a single event: subject, start and end (or `-duration`, default one hour), location,
  text, reminder, busy status and sensitivity; `-all-day` takes dates (`-end` is the last day). A time without an
  offset is a time in the **time zone** of the client (see below); `2026-10-20T09:00:00Z` or `...+02:00` says what it is. **Attendees** make it a meeting: the
  account is the organizer. **HCL Traveler creates such an entry and invites nobody** (seen on a lab server), so the client
  mails the invitation itself: a mail with a text part and a `text/calendar` part with `METHOD:REQUEST` (iTIP), which Notes and
  other calendar programs show as an invitation with the buttons to answer it. The entry and the invitation have the same `UID`.
  That is mail to other people, so it needs `-send` on top of `-write`. Deleting a meeting that you organize mails the
  cancellation (`METHOD:CANCEL`) the same way and needs `-send` too; `-no-invite-mail` and `-no-cancel-mail` (REST:
  `"no_invite_mail": true`, `?cancel_mail=false`) leave it to a server that does it itself. The answer says `invitation_mail`.
  Traveler answers the creation with the id that the client sent, not the id of the entry; the client reads the calendar again to
  find the real one. The creation is made by the client (with `Options`), because the library's requests have none. Not done:
  changing an event, answering an invitation (accept, tentative, decline), repeating events.
- **The calendar** can be read and, for single events, created and deleted. `calendar list` shows the events from now on (a running event and the upcoming ones); `-from`
  reaches into the past (a date like `2026-10-01`, or a time), `-to` ends the list. The server is asked for the date window
  that covers `-from` (`since` in the answer; 2 weeks to 6 months), and `get` looks in the widest one. A **repeating event is
  not expanded**: it comes once, with `repeat` (type, interval, days, until) and `exceptions`, and its start is the first
  instance; it is in the list while its rule runs. Times are UTC as the server sends them; an event of a time zone is not
  converted, and `all_day` events start at midnight. The body of an event is only there if the server sends it.
  Whether your Traveler sends all of this has to be tried (`-trace`).
- **The iCalendar file** (`calendar ics`) is built by the client from the fields of the event, because ActiveSync sends no
  iCalendar text. Like `eml` it is a stream and not JSON; the command line and the REST API give the same bytes, except the
  `DTSTAMP`, which is the time of the request. It has the subject, location, text, start and end (a date for an all-day
  event), organizer, attendees with role and answer, busy status (`TRANSP`, `STATUS`), sensitivity (`CLASS`), the reminder
  (`VALARM`) and the repeat rule (`RRULE`): deleted instances are `EXDATE` and changed ones are events of their own with the
  same `UID` and a `RECURRENCE-ID`. Times are UTC and there is no `VTIMEZONE`, so a calendar program shows UTC times in the
  zone it is set to. A repeat rule that is not understood is left out with a `COMMENT`. It is what the server tells, not the
  original invitation of Notes.
- **Contacts** are read only. `contacts list` and `get` give JSON (the photo only as `has_photo` and its size). The vCard files
  (`vcf`, `export`) are built by the client from the fields, like the iCalendar file, as a **vCard 3.0**, which Outlook, Apple
  and Google import: name, nickname, company and department, job title, e-mail addresses, phone numbers (home, work, mobile),
  home and work address, birthday, anniversary (`X-ANNIVERSARY`), web page, categories, instant messaging address, manager,
  the notes (`NOTE`, cut to 4096 bytes) and the photo (`PHOTO`, as the server sends it, a JPEG). Not in the card: the office location, the government id and the customer
  id of the server. The `UID` is made from the folder and the id. The address book is read in full every time, so a large one
  takes a while; the photos make `export` large. The Sync of the address book is sent by the client and not by the EAS library:
  the library sends no `Options`, and an HCL Traveler answered that with status 3 (invalid sync key) although the key was right.
- **Mail ids** are the server ids of the folder. A moved mail has a new id (`new_id` in the answer).
- **The mail file.** `eml` asks the server for the message as MIME and returns exactly those bytes, with all headers, the text and
  the attachments as the server has them. It is a stream and not JSON, so the rule of the same JSON does not apply: the
  command line and the REST API give the same bytes. A server that does not offer the MIME body type answers with an error.
  HCL Traveler does (seen on a lab server). It builds the MIME itself from the Notes document, so it is what a phone gets and
  not the original internet message: the `Message-ID` is the ActiveSync id (`10165:10207`), `Received` is one generated line,
  the headers of the original relay chain are gone, there are `X-IBM-*` headers of Domino, and the body is one HTML part,
  quoted-printable. Do not use it as proof of the original headers or the routing.
- **The folder of a mail** can be left out: `eas-client get 4:10207` finds the folder `4` in the id. A Traveler id starts with the
  id of its folder and a colon (so does an Exchange id), but that is a habit of the servers and not part of the protocol, so
  the id counts only when the part before the colon is a folder of the mailbox; otherwise the answer is `folder_required`
  and you give the folder (`get 4 4:10207`). A folder that is given always wins. REST has the same paths without the folder:
  `/accounts/{account}/messages/{id}` (and `.../reply`, `.../forward`). A list shows `folder` and `id` of every mail, so a
  program can always give both.
- **Delete** moves the mail to Deleted Items; deleting it there removes it.
- **Not part of the first version:** attachments (the list says whether there are any), HTML bodies (plain text only),
  writing to the calendar (create, change, accept an invitation) and to the address book, tasks and notes, push (`watch`). They go into the shared code first (`probe`, `client`) and
  then into the command line and the API at the same time.
- **Reply and forward** use the commands SmartReply and SmartForward: the client sends the new text and the server adds the
  original. Whether and how your Traveler quotes the original has to be seen (the mock only appends it). A reply goes to the
  sender of the original; with `-all` also to its To and Cc, the account itself left out. The subject gets `Re: ` or `Fw: `
  unless it has one. They are sent mail, so they need `-send`. Not done: replying with a different sender, attachments.
- **Tested** against `eas-mock-server -mailbox` (a small real mailbox) and by the unit tests; **not yet against a Traveler**.
  Read (the `Change` command), move, fetch, reply and forward are the operations that Traveler might answer differently: please try them
  with `-trace FILE` (every request and response, WBXML decoded, no password or cookies) on a test account first.

## Try it without Traveler

```
./eas-mock-server -listen 127.0.0.1:8099 -mailbox
export EASCLIENT_PASSWORD=secret
./eas-client -server http://127.0.0.1:8099 -email alice@example.com -no-state -send send -to alice@example.com -subject Hi -body Hello
./eas-client -server http://127.0.0.1:8099 -email alice@example.com -no-state list inbox
```

The mock delivers a mail to a user that has logged in at least once, so send to your own address, or to another user after a
first command of that user.

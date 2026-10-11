// Package load is the nshtestherd worker that generates ActiveSync load:
// one process hosts N simulated devices, each registered with the
// coordinator as its own client, each running the jobs the coordinator asks
// for.
package load

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Job names understood by the worker (the `job` of a `run` command).
const (
	JobProbe = "eas-probe" // one full flow: options, provision, folders, sync
	JobSync  = "eas-sync"  // keep the mailbox in sync until told otherwise
)

// syncParams are the parameters of an eas-sync run, in the URL style the
// coordinator carries: think=10-30&ping=60&duration=600.
type syncParams struct {
	ThinkMin, ThinkMax time.Duration // wait between syncs (push off)
	Ping               int           // heartbeat seconds; > 0 = push mode
	Duration           time.Duration // 0 = until stopped
}

func defaultSyncParams() syncParams {
	return syncParams{ThinkMin: 10 * time.Second, ThinkMax: 30 * time.Second}
}

// parseRange reads "15" or "10-30" (seconds, decimals allowed).
func parseRange(s string) (lo, hi time.Duration, err error) {
	a, b, isRange := strings.Cut(s, "-")
	x, err := strconv.ParseFloat(a, 64)
	if err != nil || x < 0 {
		return 0, 0, fmt.Errorf("invalid range %q", s)
	}
	y := x
	if isRange {
		if y, err = strconv.ParseFloat(b, 64); err != nil || y < x {
			return 0, 0, fmt.Errorf("invalid range %q", s)
		}
	}
	return time.Duration(x * float64(time.Second)), time.Duration(y * float64(time.Second)), nil
}

// parseSyncParams rejects parameters the job does not know: that fails the
// job, not the worker (worker contract).
func parseSyncParams(raw string) (syncParams, error) {
	p := defaultSyncParams()
	if raw == "" {
		return p, nil
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return p, fmt.Errorf("invalid params: %w", err)
	}
	for k, v := range vals {
		val := v[0]
		switch k {
		case "think":
			if p.ThinkMin, p.ThinkMax, err = parseRange(val); err != nil {
				return p, fmt.Errorf("think: %w", err)
			}
		case "ping":
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 || n > 3540 {
				return p, fmt.Errorf("ping: invalid heartbeat %q", val)
			}
			p.Ping = n
		case "duration":
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 {
				return p, fmt.Errorf("duration: invalid seconds %q", val)
			}
			p.Duration = time.Duration(n) * time.Second
		default:
			return p, fmt.Errorf("unknown parameter %q for %s", k, JobSync)
		}
	}
	return p, nil
}

// parseProbeParams: eas-probe takes none.
func parseProbeParams(raw string) error {
	if raw == "" {
		return nil
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return fmt.Errorf("invalid params: %w", err)
	}
	for k := range vals {
		return fmt.Errorf("unknown parameter %q for %s", k, JobProbe)
	}
	return nil
}

// think picks a random wait inside the range.
func (p syncParams) think() time.Duration {
	if p.ThinkMax <= p.ThinkMin {
		return p.ThinkMin
	}
	return p.ThinkMin + time.Duration(rand.Int64N(int64(p.ThinkMax-p.ThinkMin)+1))
}

// JobGal is the directory job.
const JobGal = "eas-gal"

// galParams are the parameters of an eas-gal run: mode=self|typeahead,
// query=TEXT (a fixed search text), think=MIN-MAX, duration=N.
type galParams struct {
	Mode               string // "self" (default): look for the own address; "typeahead": the prefixes of it, like typing
	Query              string // fixed search text; overrides the mode
	ThinkMin, ThinkMax time.Duration
	Duration           time.Duration
}

func parseGalParams(raw string) (galParams, error) {
	d := defaultSyncParams()
	p := galParams{Mode: "self", ThinkMin: d.ThinkMin, ThinkMax: d.ThinkMax}
	if raw == "" {
		return p, nil
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return p, fmt.Errorf("invalid params: %w", err)
	}
	for k, v := range vals {
		val := v[0]
		switch k {
		case "mode":
			if val != "self" && val != "typeahead" {
				return p, fmt.Errorf("mode: must be self or typeahead, not %q", val)
			}
			p.Mode = val
		case "query":
			if strings.TrimSpace(val) == "" {
				return p, errors.New("query: empty")
			}
			p.Query = val
		case "think":
			if p.ThinkMin, p.ThinkMax, err = parseRange(val); err != nil {
				return p, fmt.Errorf("think: %w", err)
			}
		case "duration":
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 {
				return p, fmt.Errorf("duration: invalid seconds %q", val)
			}
			p.Duration = time.Duration(n) * time.Second
		default:
			return p, fmt.Errorf("unknown parameter %q for %s", k, JobGal)
		}
	}
	return p, nil
}

func (p galParams) think() time.Duration {
	return syncParams{ThinkMin: p.ThinkMin, ThinkMax: p.ThinkMax}.think()
}

// JobMail is the mail job: send mail through ActiveSync and (to self) measure
// how long it takes to come back.
const JobMail = "eas-mail"

// mailParams are the parameters of an eas-mail run:
// to=self|others|a@x.de,b@y.de, think=MIN-MAX, size=MIN-MAX (KB), attach=MIN-MAX (KB each), files=N, attach_type=txt|bin, save_sent=yes|no, wait=N,
// arrival=push|poll, cleanup=yes|no, count=N, duration=N.
type mailParams struct {
	To                       string   // "self", "others" or an explicit list in List
	List                     []string // explicit recipients
	ThinkMin, ThinkMax       time.Duration
	SizeMinKB, SizeMaxKB     float64
	AttachMinKB, AttachMaxKB float64       // size of each attachment; 0 = no attachment
	Files                    int           // attachments per mail
	AttachBinary             bool          // binary attachments (.bin); default text files (.txt)
	Wait                     time.Duration // how long to wait for the mail to arrive (to=self)
	Arrival                  string        // "poll" (Sync every second, default) or "push" (Ping)
	Cleanup                  bool          // delete the mail again
	SaveSent                 bool          // keep a copy in Sent Items (default no)
	Count                    int           // 0 = until stopped
	Duration                 time.Duration
}

func defaultMailParams() mailParams {
	return mailParams{
		To:       "self",
		ThinkMin: 30 * time.Second, ThinkMax: 120 * time.Second,
		SizeMinKB: 1, SizeMaxKB: 10,
		Files:   1,
		Wait:    60 * time.Second,
		Arrival: "poll",
		Cleanup: true,
	}
}

func parseMailParams(raw string) (mailParams, error) {
	p := defaultMailParams()
	if raw == "" {
		return p, nil
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return p, fmt.Errorf("invalid params: %w", err)
	}
	for k, v := range vals {
		val := v[0]
		switch k {
		case "to":
			switch val {
			case "self", "others":
				p.To = val
			default:
				var list []string
				for _, a := range strings.Split(val, ",") {
					a = strings.TrimSpace(a)
					if !strings.Contains(a, "@") || strings.ContainsAny(a, " <>\r\n") {
						return p, fmt.Errorf("to: %q is not an e-mail address", a)
					}
					list = append(list, a)
				}
				p.To, p.List = "list", list
			}
		case "think":
			if p.ThinkMin, p.ThinkMax, err = parseRange(val); err != nil {
				return p, fmt.Errorf("think: %w", err)
			}
		case "size":
			lo, hi, err := parseRange(val)
			if err != nil {
				return p, fmt.Errorf("size: %w", err)
			}
			p.SizeMinKB, p.SizeMaxKB = lo.Seconds(), hi.Seconds()
			if p.SizeMaxKB > 10240 {
				return p, errors.New("size: at most 10240 KB")
			}
		case "attach":
			lo, hi, err := parseRange(val)
			if err != nil {
				return p, fmt.Errorf("attach: %w", err)
			}
			p.AttachMinKB, p.AttachMaxKB = lo.Seconds(), hi.Seconds()
		case "attach_type":
			switch val {
			case "txt":
				p.AttachBinary = false
			case "bin":
				p.AttachBinary = true
			default:
				return p, fmt.Errorf("attach_type: must be txt or bin, not %q", val)
			}
		case "files":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 || n > maxAttachFiles {
				return p, fmt.Errorf("files: must be 1 to %d, not %q", maxAttachFiles, val)
			}
			p.Files = n
		case "wait":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 || n > 3600 {
				return p, fmt.Errorf("wait: invalid seconds %q", val)
			}
			p.Wait = time.Duration(n) * time.Second
		case "arrival":
			if val != "push" && val != "poll" {
				return p, fmt.Errorf("arrival: must be push or poll, not %q", val)
			}
			p.Arrival = val
		case "cleanup":
			switch val {
			case "yes", "true", "1":
				p.Cleanup = true
			case "no", "false", "0":
				p.Cleanup = false
			default:
				return p, fmt.Errorf("cleanup: must be yes or no, not %q", val)
			}
		case "save_sent":
			switch val {
			case "yes", "true", "1":
				p.SaveSent = true
			case "no", "false", "0":
				p.SaveSent = false
			default:
				return p, fmt.Errorf("save_sent: must be yes or no, not %q", val)
			}
		case "count":
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 {
				return p, fmt.Errorf("count: invalid number %q", val)
			}
			p.Count = n
		case "duration":
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 {
				return p, fmt.Errorf("duration: invalid seconds %q", val)
			}
			p.Duration = time.Duration(n) * time.Second
		default:
			return p, fmt.Errorf("unknown parameter %q for %s", k, JobMail)
		}
	}
	if total := p.SizeMaxKB + p.AttachMaxKB*float64(p.Files); total > maxMailKB {
		return p, fmt.Errorf("size and attachments: %.0f KB in total, at most %d KB", total, maxMailKB)
	}
	return p, nil
}

func (p mailParams) think() time.Duration {
	return syncParams{ThinkMin: p.ThinkMin, ThinkMax: p.ThinkMax}.think()
}

// sizeBytes picks a random message size.
func (p mailParams) sizeBytes() int {
	kb := p.SizeMinKB
	if p.SizeMaxKB > p.SizeMinKB {
		kb += rand.Float64() * (p.SizeMaxKB - p.SizeMinKB)
	}
	return int(kb * 1024)
}

// splitSim takes the sim=... parameter out of a job's parameters. It is the
// value of the X-Mock-Sim header for the mock server (eas-mock-server -sim),
// for example "sim=cmd=Sync,status=3,count=1"; the pairs are separated by
// commas here because a semicolon cannot be part of a query string.
func splitSim(raw string) (rest, sim string, err error) {
	if raw == "" {
		return "", "", nil
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return raw, "", fmt.Errorf("invalid params: %w", err)
	}
	v, ok := vals["sim"]
	if !ok {
		return raw, "", nil
	}
	delete(vals, "sim")
	return vals.Encode(), strings.ReplaceAll(v[0], ",", ";"), nil
}

// maxAttachFiles and maxMailKB limit the attachments of one test mail: its text and all attachments together.
const (
	maxAttachFiles = 10
	maxMailKB      = 10240
)

// attachBytes picks a random size for each attachment of a mail; 0 when the job sends no attachments.
func (p mailParams) attachBytes() int {
	kb := p.AttachMinKB
	if p.AttachMaxKB > p.AttachMinKB {
		kb += rand.Float64() * (p.AttachMaxKB - p.AttachMinKB)
	}
	return int(kb * 1024)
}

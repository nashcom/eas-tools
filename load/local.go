package load

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nashcom/eas-tools/herd"
)

// Local is the coordinator of a standalone run, built into the worker: it hands
// out the given accounts, starts one job on all clients as soon as every client
// has registered, and stops them when every job has ended or the duration is
// over. Jobs, statistics and metrics are the same code as with nshtestherd.
type Local struct {
	mu       sync.Mutex
	accounts []herd.Reply
	clients  int
	job      string
	params   string
	duration time.Duration

	next      int
	started   bool
	startedAt time.Time
	stopped   bool
	ended     map[int]string // test_id -> result of the job
}

const (
	localRunID  = 1
	localStopID = 2
)

// NewLocal returns the coordinator of a run of the job with the parameters on
// this many clients, one account each. A duration above zero stops the job
// after that time.
func NewLocal(accounts []herd.Reply, clients int, job, params string, duration time.Duration) *Local {
	return &Local{accounts: accounts, clients: clients, job: job, params: params, duration: duration,
		ended: map[int]string{}}
}

// Register hands out the next account.
func (l *Local) Register(_ context.Context, _ string) (*herd.Reply, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.next >= len(l.accounts) || l.next >= l.clients {
		return nil, &herd.APIError{Status: http.StatusConflict, Code: "pool_exhausted", Message: "no more accounts"}
	}
	a := l.accounts[l.next]
	l.next++
	a.TestID = l.next
	a.Command = "idle"
	return &a, nil
}

// Status records what the client reports and answers with the current command:
// idle until every client is registered, then run, then stop.
func (l *Local) Status(_ context.Context, r herd.Report) (*herd.Reply, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if r.LastJobID == localRunID && r.LastJobResult != "" {
		l.ended[r.TestID] = r.LastJobResult
	}
	if r.State == "error" {
		if _, ok := l.ended[r.TestID]; !ok {
			l.ended[r.TestID] = "failed"
		}
	}

	switch {
	case !l.started && l.next >= l.clients:
		l.started = true
		l.startedAt = time.Now()
	case l.started && !l.stopped && (len(l.ended) >= l.clients || (l.duration > 0 && time.Since(l.startedAt) >= l.duration)):
		l.stopped = true
	}

	rep := &herd.Reply{TestID: r.TestID, Command: "idle"}
	switch {
	case l.stopped:
		rep.Command, rep.CommandID = "stop", localStopID
	case l.started:
		rep.Command, rep.CommandID, rep.Job, rep.Params = "run", localRunID, l.job, l.params
	}
	return rep, nil
}

// LocalResult counts how the jobs of the clients ended.
type LocalResult struct {
	OK, Failed, Stopped int
	Missing             int // clients that never reported a result
}

// Result returns the outcome of the run: a stop by duration or by Ctrl-C is not
// a failure.
func (l *Local) Result() LocalResult {
	l.mu.Lock()
	defer l.mu.Unlock()

	var r LocalResult
	for _, res := range l.ended {
		switch res {
		case "ok":
			r.OK++
		case "stopped":
			r.Stopped++
		default:
			r.Failed++
		}
	}
	r.Missing = l.clients - len(l.ended)
	return r
}

// Success reports whether no job failed and every client reported a result.
func (r LocalResult) Success() bool {
	return r.Failed == 0 && r.Missing == 0
}

// ParseAccounts reads accounts in the nshtestherd CSV format: FirstName,
// LastName, Password, Shortname, InternetAddress, with or without the header
// line; quoted fields are supported.
func ParseAccounts(r io.Reader) ([]herd.Reply, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = 5
	cr.TrimLeadingSpace = true
	cr.Comment = '#'

	var out []herd.Reply
	for line := 1; ; line++ {
		f, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(out) == 0 && line == 1 && strings.EqualFold(strings.TrimSpace(f[0]), "firstname") {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		if f[2] == "" || f[3] == "" || !strings.Contains(f[4], "@") {
			return nil, fmt.Errorf("line %d: password, shortname and an internet address are required", line)
		}
		out = append(out, herd.Reply{Firstname: f[0], Lastname: f[1], Password: f[2], Shortname: f[3], InternetAddress: f[4]})
	}
	if len(out) == 0 {
		return nil, errors.New("no accounts")
	}
	return out, nil
}

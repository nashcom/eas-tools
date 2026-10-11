// Package easherd is a tiny nshtestherd coordinator for tests: it hands
// out accounts, records every status report and lets the test set the
// desired command.
package testherd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Report is one recorded POST /status.
type Report struct {
	TestID        int
	State         string
	Ack           int
	Message       string
	LastJobID     int
	LastJob       string
	LastJobResult string
}

// Server is the nshtestherd-compatible coordinator; it implements http.Handler.
type Server struct {
	mu       sync.Mutex
	password string
	accounts int       // size of the pool
	list     []Account // explicit accounts (NewWithAccounts); empty = generated
	keys     map[string]int
	next     int

	cmd       string
	cmdID     int
	job       string
	params    string
	pause     int
	reports   []Report
	token     string
	forget    bool // answer 404 unknown_test_id
	exhausted int  // number of register calls answered with pool_exhausted first

	latest  map[int]Report // latest report per client
	counted map[int]int    // last_job_id already counted per client
	jobs    map[string]int // ok | failed | stopped
	started time.Time

	// MaxReports caps the kept report history (0 = keep all, what the tests
	// want; a long-running demo sets a limit).
	MaxReports int
}

// Account is one account of the pool.
type Account struct {
	Firstname, Lastname, Password, Shortname, InternetAddress string
}

// NewWithAccounts returns a coordinator that hands out exactly these accounts.
func NewWithAccounts(list []Account) *Server {
	s := New(len(list), "")
	s.list = list
	return s
}

// ParseCSV reads accounts in the nshtestherd format: five columns
// (firstname, lastname, password, shortname, internetaddress), with or
// without a header line, empty lines ignored.
func ParseCSV(text string) ([]Account, error) {
	var out []Account
	for n, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) != 5 {
			return nil, fmt.Errorf("line %d: expected 5 columns, found %d", n+1, len(f))
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		if n == 0 && strings.EqualFold(f[0], "firstname") {
			continue
		}
		if f[2] == "" || f[3] == "" || !strings.Contains(f[4], "@") {
			return nil, fmt.Errorf("line %d: password, shortname and an internet address are required", n+1)
		}
		out = append(out, Account{f[0], f[1], f[2], f[3], f[4]})
	}
	if len(out) == 0 {
		return nil, errors.New("no accounts")
	}
	return out, nil
}

// New returns a coordinator with the given pool size; every account has
// the given password and the internet address loadNNNNNN@example.com.
func New(accounts int, password string) *Server {
	return &Server{
		accounts: accounts, password: password, keys: map[string]int{}, cmd: "idle",
		latest: map[int]Report{}, counted: map[int]int{}, jobs: map[string]int{}, started: time.Now(),
	}
}

// RequireToken makes the server check the bearer token.
func (s *Server) RequireToken(t string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.token = t
}

// ExhaustFirst answers the first n register calls with 409 pool_exhausted.
func (s *Server) ExhaustFirst(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.exhausted = n
}

// Forget makes every /status answer 404 unknown_test_id (coordinator restart).
func (s *Server) Forget() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.forget = true
}

// Command sets the desired command for all clients and returns its id.
func (s *Server) Command(cmd, job, params string, pauseSeconds int) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cmdID++
	s.cmd, s.job, s.params, s.pause = cmd, job, params, pauseSeconds
	return s.cmdID
}

// Reports returns a copy of all recorded status reports.
func (s *Server) Reports() []Report {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]Report(nil), s.reports...)
}

// Last returns the latest report of a client.
func (s *Server) Last(testID int) (Report, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := len(s.reports) - 1; i >= 0; i-- {
		if s.reports[i].TestID == testID {
			return s.reports[i], true
		}
	}
	return Report{}, false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

func (s *Server) instruction(testID int) map[string]any {
	return map[string]any{
		"test_id": testID, "command": s.cmd, "command_id": s.cmdID,
		"job": s.job, "params": s.params, "pause_seconds": s.pause,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.Method == http.MethodGet && r.URL.Path == "/health" {
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
		apiError(w, 401, "unauthorized", "token missing or wrong")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/status" {
		s.summary(w)
		return
	}
	if r.Method != http.MethodPost {
		apiError(w, 405, "method_not_allowed", "POST only")
		return
	}
	if err := r.ParseForm(); err != nil {
		apiError(w, 400, "invalid_request", err.Error())
		return
	}

	switch r.URL.Path {
	case "/register":
		if s.exhausted > 0 {
			s.exhausted--
			apiError(w, 409, "pool_exhausted", "all accounts are allocated")
			return
		}
		key := r.PostForm.Get("request_key")
		id, known := s.keys[key]
		if !known {
			if s.next >= s.accounts {
				apiError(w, 409, "pool_exhausted", "all accounts are allocated")
				return
			}
			s.next++
			id = s.next
			s.keys[key] = id
		}
		reply := s.instruction(id)
		if len(s.list) > 0 {
			a := s.list[id-1]
			reply["firstname"], reply["lastname"], reply["password"] = a.Firstname, a.Lastname, a.Password
			reply["shortname"], reply["internetaddress"] = a.Shortname, a.InternetAddress
		} else {
			reply["firstname"] = "Load"
			reply["lastname"] = fmt.Sprintf("%06d", id)
			reply["password"] = s.password
			reply["shortname"] = fmt.Sprintf("load%06d", id)
			reply["internetaddress"] = fmt.Sprintf("load%06d@example.com", id)
		}
		reply["worker_options"] = ""
		status := 201
		if known {
			status = 200
		}
		writeJSON(w, status, reply)

	case "/status":
		if s.forget {
			apiError(w, 404, "unknown_test_id", "no client with that test_id")
			return
		}
		id, _ := strconv.Atoi(r.PostForm.Get("test_id"))
		ack, _ := strconv.Atoi(r.PostForm.Get("ack_command_id"))
		lj, _ := strconv.Atoi(r.PostForm.Get("last_job_id"))
		rep := Report{
			TestID: id, State: r.PostForm.Get("state"), Ack: ack, Message: r.PostForm.Get("message"),
			LastJobID: lj, LastJob: r.PostForm.Get("last_job"), LastJobResult: r.PostForm.Get("last_job_result"),
		}
		s.reports = append(s.reports, rep)
		if s.MaxReports > 0 && len(s.reports) > s.MaxReports {
			s.reports = append([]Report(nil), s.reports[len(s.reports)-s.MaxReports:]...)
		}
		s.latest[id] = rep
		if lj > s.counted[id] {
			s.counted[id] = lj
			s.jobs[rep.LastJobResult]++
		}
		writeJSON(w, 200, s.instruction(id))

	case "/command":
		cmd := r.PostForm.Get("command")
		switch cmd {
		case "idle", "stop":
		case "run":
			if r.PostForm.Get("job") == "" {
				apiError(w, 400, "invalid_request", "run requires job")
				return
			}
		case "pause":
			if n, err := strconv.Atoi(r.PostForm.Get("pause_seconds")); err != nil || n < 1 {
				apiError(w, 400, "invalid_request", "pause requires pause_seconds")
				return
			}
		default:
			apiError(w, 400, "invalid_request", "unknown command")
			return
		}
		if r.PostForm.Get("target") != "all" {
			apiError(w, 400, "invalid_request", "this coordinator only supports target=all")
			return
		}
		pause, _ := strconv.Atoi(r.PostForm.Get("pause_seconds"))
		s.cmdID++
		s.cmd, s.job, s.params, s.pause = cmd, r.PostForm.Get("job"), r.PostForm.Get("params"), pause
		writeJSON(w, 200, map[string]any{"target": "all", "command": cmd, "command_id": s.cmdID, "updated": len(s.keys)})

	default:
		apiError(w, 404, "not_found", "unknown path")
	}
}

// summary answers GET /status like the real coordinator, with fewer fields.
func (s *Server) summary(w http.ResponseWriter) {
	states := map[string]int{}
	for _, r := range s.latest {
		states[r.State]++
	}
	out := map[string]any{
		"users_total":     s.accounts,
		"users_allocated": len(s.keys),
		"users_available": s.accounts - len(s.keys),
		"clients_total":   len(s.keys),
		"jobs_ok":         s.jobs["ok"],
		"jobs_failed":     s.jobs["failed"],
		"jobs_stopped":    s.jobs["stopped"],
		"uptime_seconds":  int(time.Since(s.started).Seconds()),
	}
	names := make([]string, 0, len(states))
	for n := range states {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out["clients_"+n] = states[n]
	}
	writeJSON(w, 200, out)
}

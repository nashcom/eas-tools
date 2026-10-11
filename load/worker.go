package load

import (
	"context"
	"errors"
	"fmt"
	"github.com/nashcom/eas-tools/internal/logging"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nashcom/eas-tools/herd"
	"github.com/nashcom/eas-tools/probe"
)

// Config configures a worker process.
type Config struct {
	Herd Coordinator
	// Name is the prefix of the request keys ("<Name>-<index>"). Keep it
	// stable across restarts so a restarted worker gets its accounts back,
	// and different between worker processes that run in parallel.
	Name    string
	Clients int

	Server string // ActiveSync endpoint (host or URL)
	Login  string // "internetaddress" (default) or "shortname"
	// Devices is how many devices each client (account) runs at the same
	// time, like a user with a phone and a tablet (default 1). The profiles
	// are used in turn: Devices=3 with Profiles=[iphone ipad] gives
	// iphone, ipad, iphone.
	Devices   int
	Profiles  []string // default [iphone]
	Provision string   // auto | always | never
	Insecure  bool
	CAFile    string
	// Headers are extra request headers for every request of every device
	// (see probe.ParseHeaders), for example X-Mock-Sim.
	Headers http.Header

	// Store keeps policy and sync keys of all devices.
	Store *probe.Store

	Poll        time.Duration // status poll interval (default 2s)
	Ramp        time.Duration // delay between client starts (default 100ms)
	RetryDelay  time.Duration // wait when the coordinator has no account yet (default 5s)
	StepTimeout time.Duration // timeout of one ActiveSync request step (default 30s)

	Stats *Stats
	Log   *slog.Logger
	// SessionLog receives the ActiveSync library's debug output; discarded
	// when nil.
	SessionLog *slog.Logger
}

func (c *Config) defaults() error {
	if c.Herd == nil {
		return errors.New("coordinator client is required")
	}
	if c.Server == "" {
		return errors.New("server is required")
	}
	if c.Clients <= 0 {
		return errors.New("clients must be at least 1")
	}
	if c.Name == "" {
		return errors.New("name is required")
	}
	if c.Login == "" {
		c.Login = "internetaddress"
	}
	if c.Login != "internetaddress" && c.Login != "shortname" {
		return fmt.Errorf("login must be internetaddress or shortname, not %q", c.Login)
	}
	if c.Devices <= 0 {
		c.Devices = 1
	}
	if len(c.Profiles) == 0 {
		c.Profiles = []string{probe.DefaultProfile}
	}
	for _, p := range c.Profiles {
		if _, ok := probe.LookupProfile(p); !ok {
			return fmt.Errorf("unknown profile %q (available: %s)", p, strings.Join(probe.ProfileNames(), ", "))
		}
	}
	if c.Poll <= 0 {
		c.Poll = 2 * time.Second
	}
	if c.Ramp < 0 {
		c.Ramp = 0
	}
	if c.RetryDelay <= 0 {
		c.RetryDelay = 5 * time.Second
	}
	if c.StepTimeout <= 0 {
		c.StepTimeout = 30 * time.Second
	}
	if c.Stats == nil {
		c.Stats = NewStats()
	}
	if c.Log == nil {
		c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.SessionLog == nil {
		c.SessionLog = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.Store == nil {
		c.Store, _ = probe.OpenStore("")
	}
	return nil
}

// Run starts the clients (one goroutine each, ramped up) and returns when
// all of them have ended: after a `stop` command, a coordinator restart, or
// when ctx is cancelled (the worker's own shutdown).
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.defaults(); err != nil {
		return err
	}

	var wg sync.WaitGroup
	for i := 0; i < cfg.Clients; i++ {
		if ctx.Err() != nil {
			break
		}
		c := &client{
			cfg:   &cfg,
			index: i + 1,
			key:   fmt.Sprintf("%s-%d", cfg.Name, i+1),
		}
		c.log = cfg.Log.With("client", c.key)

		wg.Add(1)
		go func() {
			defer wg.Done()
			c.run(ctx)
		}()

		if cfg.Ramp > 0 && i+1 < cfg.Clients {
			sleep(ctx, cfg.Ramp)
		}
	}
	wg.Wait()
	return nil
}

// client is one simulated device: one coordinator registration, one ActiveSync
// session, at most one running job.
type client struct {
	cfg   *Config
	index int
	key   string
	log   *slog.Logger

	testID  int
	account *herd.Reply
	devs    []*device

	// owned by the poll loop
	applied   int // last applied command_id
	reported  string
	job       *job
	stopping  bool
	lastJobID int
	lastJob   string
	lastRes   string
	lastMsg   string

	// shared with the job goroutine
	mu         sync.Mutex
	pauseUntil time.Time
}

type job struct {
	id     int
	name   string
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	statuses map[int]string // status text per device number

	// written by the job goroutine before done is closed
	result  string
	message string
}

func (j *job) setStatus(device int, s string) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.statuses[device] = s
}

// getStatus is the status text of the job; with several devices every
// device has its own part.
func (j *job) getStatus() string {
	j.mu.Lock()
	defer j.mu.Unlock()

	if len(j.statuses) == 0 {
		return j.name + ": starting"
	}
	nums := make([]int, 0, len(j.statuses))
	for n := range j.statuses {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	if len(nums) == 1 {
		return j.statuses[nums[0]]
	}
	parts := make([]string, 0, len(nums))
	for _, n := range nums {
		parts = append(parts, fmt.Sprintf("d%d %s", n, strings.TrimPrefix(j.statuses[n], j.name+": ")))
	}
	return j.name + ": " + strings.Join(parts, " | ")
}

// sleep waits for d or until ctx ends; it reports whether ctx is still alive.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (c *client) run(ctx context.Context) {
	if !c.register(ctx) {
		return
	}
	if !c.openSession() {
		return
	}

	for {
		if ctx.Err() != nil {
			c.shutdown("worker shutdown")
			return
		}

		c.collectJobResult()

		reply, err := c.report(ctx, c.state(), c.message())
		switch {
		case err == nil:
			c.apply(reply)
		case herd.UnknownClient(err):
			c.log.Error("coordinator does not know this client (restarted?); ending", "test_id", c.testID)
			c.setReported("error")
			c.stopJob()
			return
		case ctx.Err() != nil:
			continue
		default:
			c.log.Warn("status report failed", "error", err)
		}

		if c.stopping {
			_, _ = c.report(ctx, "stopping", "stopping")
			c.finish("finished")
			return
		}

		// A little jitter keeps thousands of clients from polling in lockstep.
		wait := c.cfg.Poll + time.Duration(rand.Int64N(int64(c.cfg.Poll)/5+1))
		sleep(ctx, wait)
	}
}

// register gets an account, waiting while the pool has none. It reports
// false when the worker should end.
func (c *client) register(ctx context.Context) bool {
	for {
		reply, err := c.cfg.Herd.Register(ctx, c.key)
		if err == nil {
			c.testID = reply.TestID
			c.applied = 0
			c.log = c.log.With("test_id", reply.TestID, "user", reply.Shortname)
			c.account = reply
			logging.Trace(c.log, "registered")
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		if herd.WaitForAccount(err) {
			c.log.Info("no account available yet, waiting", "reason", err)
		} else {
			c.log.Warn("register failed, retrying", "error", err)
		}
		if !sleep(ctx, c.cfg.RetryDelay) {
			return false
		}
	}
}

// openSession builds one ActiveSync session per device of the account. An
// account that cannot be used at all is a final error, reported to the
// coordinator.
func (c *client) openSession() bool {
	acct := c.account
	user := acct.InternetAddress
	if c.cfg.Login == "shortname" {
		user = acct.Shortname
	}

	for n := 1; n <= c.cfg.Devices; n++ {
		sess, err := probe.NewSession(probe.Config{
			ServerURL:    c.cfg.Server,
			Username:     user,
			Email:        acct.InternetAddress,
			Password:     acct.Password,
			Profile:      c.cfg.Profiles[(n-1)%len(c.cfg.Profiles)],
			DeviceNumber: n,
			Provision:    c.cfg.Provision,
			Store:        c.cfg.Store,
			Insecure:     c.cfg.Insecure,
			CACertFile:   c.cfg.CAFile,
			StepTimeout:  c.cfg.StepTimeout,
			OnExchange:   c.cfg.Stats.Observe,
			Headers:      c.cfg.Headers,
			Logger:       c.cfg.SessionLog,
		})
		if err != nil {
			c.log.Error("cannot create session", "device", n, "error", err)
			_, _ = c.report(context.Background(), "error", fmt.Sprintf("session %d: %v", n, err))
			c.setReported("error")
			c.closeSessions()
			return false
		}
		c.devs = append(c.devs, &device{c: c, number: n, sess: sess})
	}
	return true
}

func (c *client) closeSessions() {
	for _, d := range c.devs {
		d.sess.Close()
	}
}

// state computes the state to report.
func (c *client) state() string {
	switch {
	case c.stopping:
		return "stopping"
	case c.paused():
		return "paused"
	case c.job != nil:
		return "running"
	}
	return "idle"
}

// message is the status text sent with the report.
func (c *client) message() string {
	if c.job != nil {
		return c.job.getStatus()
	}
	return c.lastMsg
}

func (c *client) setReported(state string) {
	if state == c.reported {
		return
	}
	c.cfg.Stats.ClientState(c.reported, state)
	c.reported = state
}

func (c *client) report(ctx context.Context, state, message string) (*herd.Reply, error) {
	reply, err := c.cfg.Herd.Status(ctx, herd.Report{
		TestID:        c.testID,
		State:         state,
		Ack:           c.applied,
		Message:       message,
		LastJobID:     c.lastJobID,
		LastJob:       c.lastJob,
		LastJobResult: c.lastRes,
	})
	if err == nil {
		c.setReported(state)
	}
	return reply, err
}

// apply carries out an instruction. Each command_id is applied exactly
// once; a repeated id (every poll repeats the current one) does nothing.
func (c *client) apply(r *herd.Reply) {
	if r.CommandID == c.applied {
		return
	}
	c.applied = r.CommandID
	logging.Trace(c.log, "command", "id", r.CommandID, "command", r.Command, "job", r.Job, "params", r.Params)

	switch r.Command {
	case "idle":
		c.stopJob()
	case "run":
		c.stopJob()
		c.startJob(r.CommandID, r.Job, r.Params)
	case "pause":
		c.mu.Lock()
		c.pauseUntil = time.Now().Add(time.Duration(r.PauseSeconds) * time.Second)
		c.mu.Unlock()
	case "stop":
		c.stopJob()
		c.stopping = true
	}
}

func (c *client) paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return time.Now().Before(c.pauseUntil)
}

// gate blocks while a pause is in effect (jobs call it between operations,
// the safe boundaries the contract asks for). It returns false when ctx ends.
func (c *client) gate(ctx context.Context) bool {
	for {
		c.mu.Lock()
		d := time.Until(c.pauseUntil)
		c.mu.Unlock()
		if d <= 0 {
			return ctx.Err() == nil
		}
		if !sleep(ctx, d) {
			return false
		}
	}
}

// startJob runs a job in its own goroutine; the poll loop keeps reporting.
func (c *client) startJob(id int, name, params string) {
	jctx, cancel := context.WithCancel(context.Background())
	j := &job{id: id, name: name, cancel: cancel, done: make(chan struct{}), statuses: map[int]string{}}
	c.job = j

	go func() {
		defer close(j.done)
		defer cancel()
		j.result, j.message = c.runJob(jctx, j, params)
	}()
}

// stopJob ends the running job and records its result.
func (c *client) stopJob() {
	if c.job == nil {
		return
	}
	c.job.cancel()
	<-c.job.done
	c.recordJob(c.job)
	c.job = nil
}

// collectJobResult notices a job that ended by itself.
func (c *client) collectJobResult() {
	if c.job == nil {
		return
	}
	select {
	case <-c.job.done:
		c.recordJob(c.job)
		c.job = nil
	default:
	}
}

func (c *client) recordJob(j *job) {
	c.lastJobID, c.lastJob, c.lastRes, c.lastMsg = j.id, j.name, j.result, j.message
	c.cfg.Stats.Job(j.name, j.result)
	logging.Trace(c.log, "job ended", "job", j.name, "result", j.result, "message", j.message)
}

// finish reports the end of a stopped worker: stopping was reported by the
// loop, now done.
func (c *client) finish(msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _ = c.report(ctx, "done", msg)
	c.closeSessions()
}

// shutdown handles the worker's own end (signal): the job ends as stopped
// and the client reports done.
func (c *client) shutdown(msg string) {
	c.stopJob()
	c.finish(msg)
}

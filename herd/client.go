// Package herd is a client for the nshtestherd coordinator (protocol v1,
// see nshtestherd/docs/PROTOCOL.md): a worker registers to get an account
// and a test_id, then polls /status to report its state and to receive the
// desired command.
package herd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxMessageBytes is how much of a status message is sent. The coordinator
// accepts 2048 bytes; staying below leaves room for escaping.
const MaxMessageBytes = 1024

// Reply is the flat object the coordinator returns from /register and
// /status. /status answers only the instruction fields.
type Reply struct {
	TestID          int    `json:"test_id"`
	Firstname       string `json:"firstname"`
	Lastname        string `json:"lastname"`
	Password        string `json:"password"`
	Shortname       string `json:"shortname"`
	InternetAddress string `json:"internetaddress"`
	WorkerOptions   string `json:"worker_options"`

	Command      string `json:"command"` // idle | run | pause | stop
	CommandID    int    `json:"command_id"`
	Job          string `json:"job"`
	Params       string `json:"params"`
	PauseSeconds int    `json:"pause_seconds"`
}

// Report is what a worker tells the coordinator on every poll.
type Report struct {
	TestID int
	// State is one of idle running paused stopping done error.
	State string
	// Ack is the last applied command_id.
	Ack     int
	Message string

	// The last job that ended; all three go together and are resent with
	// every report (the coordinator counts each id once).
	LastJobID     int
	LastJob       string
	LastJobResult string // ok | failed | stopped
}

// APIError is an error answer of the coordinator.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("herd: HTTP %d %s: %s", e.Status, e.Code, e.Message)
}

// WaitForAccount reports whether the error means "no account yet, try again":
// the pool is exhausted or not loaded.
func WaitForAccount(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusConflict &&
		(ae.Code == "pool_exhausted" || ae.Code == "no_accounts_loaded")
}

// UnknownClient reports whether the coordinator does not know the test_id
// (it was restarted), which ends the worker.
func UnknownClient(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound && ae.Code == "unknown_test_id"
}

// Client talks to one coordinator.
type Client struct {
	Base  string // e.g. http://127.0.0.1:8788
	Token string // optional bearer token (NSHTEST_TOKEN)
	HTTP  *http.Client
}

// New returns a client with a short request timeout (each request is one
// quick call; nothing in the protocol blocks).
func New(base, token string) *Client {
	return &Client{
		Base:  strings.TrimRight(base, "/"),
		Token: token,
		HTTP:  &http.Client{Timeout: 15 * time.Second},
	}
}

// Register asks for an account. The request key makes the call idempotent:
// the same key returns the same account and test_id.
func (c *Client) Register(ctx context.Context, requestKey string) (*Reply, error) {
	form := url.Values{}
	form.Set("request_key", requestKey)
	return c.post(ctx, "/register", form)
}

// Status reports the worker's state and returns the current instruction.
func (c *Client) Status(ctx context.Context, r Report) (*Reply, error) {
	form := url.Values{}
	form.Set("test_id", strconv.Itoa(r.TestID))
	form.Set("state", r.State)
	form.Set("ack_command_id", strconv.Itoa(r.Ack))
	if m := cleanMessage(r.Message); m != "" {
		form.Set("message", m)
	}
	if r.LastJobID > 0 {
		form.Set("last_job_id", strconv.Itoa(r.LastJobID))
		form.Set("last_job", r.LastJob)
		form.Set("last_job_result", r.LastJobResult)
	}
	return c.post(ctx, "/status", form)
}

func (c *Client) post(ctx context.Context, path string, form url.Values) (*Reply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		ae := &APIError{Status: resp.StatusCode}
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			ae.Code, ae.Message = e.Error, e.Message
		} else {
			ae.Code = http.StatusText(resp.StatusCode)
			ae.Message = strings.TrimSpace(string(bytes.TrimSpace(body)))
		}
		return nil, ae
	}

	var out Reply
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("herd: bad reply from %s: %w", path, err)
	}
	return &out, nil
}

// cleanMessage makes a status message safe for the protocol: printable,
// single line, cut at a UTF-8 boundary below MaxMessageBytes.
func cleanMessage(m string) string {
	m = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, m)
	m = strings.TrimSpace(m)
	if len(m) <= MaxMessageBytes {
		return m
	}
	cut := MaxMessageBytes
	for cut > 0 && !utf8.RuneStart(m[cut]) {
		cut--
	}
	return m[:cut]
}

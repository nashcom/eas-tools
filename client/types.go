// Package client is the ActiveSync mail client behind eas-client. It offers the operations of a mail client (folders,
// list, get, send, read, move, delete) on named accounts. The command line and the REST API both call it and print what
// it returns: the types of this file are the JSON of both, so a command and the request of the same name give the same
// answer, and an error has the same body in both.
package client

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Error is a failed operation. Code is the stable name for programs, Message the text for people, Status the HTTP
// status that the REST API answers with.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"-"`
}

func (e *Error) Error() string { return e.Message }

// ErrorBody is what both interfaces print for an error.
type ErrorBody struct {
	Error *Error `json:"error"`
}

func newError(status int, code, message string) *Error {
	return &Error{Code: code, Message: message, Status: status}
}

// AsError returns err as an *Error (an error that is none becomes code "error", HTTP 500).
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return newError(http.StatusInternalServerError, "error", err.Error())
}

// Encode writes a result or an error body as JSON, indented, followed by a new line. The command line and the REST API
// both use it, which is what makes their output the same.
func Encode(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// Time is a time in the two forms the tools use everywhere: readable UTC with all the digits the clock has, and Unix
// nanoseconds in a string, like time_unix_nano of the log.
type Time struct {
	Time     string `json:"time"`
	UnixNano string `json:"unix_nano"`
}

func stamp(t time.Time) *Time {
	if t.IsZero() {
		return nil
	}
	return &Time{Time: t.UTC().Format(time.RFC3339Nano), UnixNano: strconv.FormatInt(t.UnixNano(), 10)}
}

// AccountInfo is an account of the configuration (never its password).
type AccountInfo struct {
	Name   string `json:"name"`
	Server string `json:"server"`
	Email  string `json:"email,omitempty"`
	User   string `json:"user,omitempty"`
}

// AccountsResult is the answer of "accounts".
type AccountsResult struct {
	Accounts   []AccountInfo `json:"accounts"`
	AllowWrite bool          `json:"allow_write"`
	AllowSend  bool          `json:"allow_send"`
}

// Info is the answer of "info": what the server and the device are.
type Info struct {
	Account string     `json:"account"`
	User    string     `json:"user"`
	Email   string     `json:"email,omitempty"`
	Server  ServerInfo `json:"server"`
	Device  DeviceInfo `json:"device"`
	Folders int        `json:"folders"`
	// TimeZone is the zone of the account: times without an offset are in it.
	TimeZone string `json:"time_zone"`
}

type ServerInfo struct {
	URL             string   `json:"url"`
	Product         string   `json:"product,omitempty"`
	ProtocolVersion string   `json:"protocol_version,omitempty"`
	Versions        []string `json:"versions,omitempty"`
	Commands        []string `json:"commands,omitempty"`
	Policy          string   `json:"policy,omitempty"`
}

type DeviceInfo struct {
	ID          string `json:"id"`
	Provisioned bool   `json:"provisioned"`
}

// Folder is a folder of the mailbox.
type Folder struct {
	ID     string `json:"id"`
	Parent string `json:"parent,omitempty"`
	Name   string `json:"name"`
	Type   string `json:"type"`
}

// FoldersResult is the answer of "folders".
type FoldersResult struct {
	Account string   `json:"account"`
	Folders []Folder `json:"folders"`
}

// Message is a mail. In a list the body is only the start of it (body_truncated says so); "get" has the whole body.
type Message struct {
	ID             string `json:"id"`
	Folder         string `json:"folder"`
	Subject        string `json:"subject"`
	From           string `json:"from,omitempty"`
	To             string `json:"to,omitempty"`
	Cc             string `json:"cc,omitempty"`
	Date           *Time  `json:"date,omitempty"`
	Read           bool   `json:"read"`
	Flagged        bool   `json:"flagged"`
	Importance     string `json:"importance"`
	HasAttachments bool   `json:"has_attachments"`
	Body           string `json:"body,omitempty"`
	BodySize       int    `json:"body_size"`
	BodyTruncated  bool   `json:"body_truncated"`
}

// ListResult is the answer of "list".
type ListResult struct {
	Account string `json:"account"`
	Folder  Folder `json:"folder"`
	// Since is the date window of the list. A list can only show the mail of that window: an old mail in a folder is not in it.
	Since    string    `json:"since"`
	Count    int       `json:"count"`
	Messages []Message `json:"messages"`
}

// GetResult is the answer of "get".
type GetResult struct {
	Account string  `json:"account"`
	Message Message `json:"message"`
}

// SendRequest is the mail to send, as the REST API receives it and as the command line builds it.
type SendRequest struct {
	To      []string `json:"to"`
	Cc      []string `json:"cc,omitempty"`
	Subject string   `json:"subject"`
	Body    string   `json:"body"`
	// SaveInSent keeps a copy in Sent Items; the default is yes.
	SaveInSent *bool `json:"save_in_sent,omitempty"`
}

// ReplyRequest is a reply, as the REST API receives it and as the command line builds it. The server adds the original
// mail to the text (SmartReply).
type ReplyRequest struct {
	Body string `json:"body"`
	// All sends the reply to all recipients of the mail, not only to its sender.
	All        bool  `json:"all,omitempty"`
	SaveInSent *bool `json:"save_in_sent,omitempty"`
}

// ForwardRequest is a forward of a mail (SmartForward).
type ForwardRequest struct {
	To         []string `json:"to"`
	Cc         []string `json:"cc,omitempty"`
	Body       string   `json:"body"`
	SaveInSent *bool    `json:"save_in_sent,omitempty"`
}

// MailRef points at a mail.
type MailRef struct {
	Folder string `json:"folder"`
	ID     string `json:"id"`
}

// SendResult is the answer of "send", "reply" and "forward".
type SendResult struct {
	Account string `json:"account"`
	// Action is send, reply or forward; for a reply and a forward Source is the mail it is about.
	Action  string   `json:"action"`
	Source  *MailRef `json:"source,omitempty"`
	From    string   `json:"from"`
	To      []string `json:"to"`
	Cc      []string `json:"cc,omitempty"`
	Subject string   `json:"subject"`
	Sent    bool     `json:"sent"`
	Time    *Time    `json:"time"`
}

// ChangeResult is the answer of "read", "unread", "move" and "delete".
type ChangeResult struct {
	Account string `json:"account"`
	Action  string `json:"action"` // read, unread, move, delete
	Folder  string `json:"folder"`
	ID      string `json:"id"`
	// To and NewID tell where a moved mail went and how it is called there.
	To    string `json:"to,omitempty"`
	NewID string `json:"new_id,omitempty"`
	// Attendees are the people an event was created for; the server invites them (or not: that is its decision).
	Attendees []string `json:"attendees,omitempty"`
	// InvitationMail is true when the client mailed the invitation (or the cancellation) of a meeting to its attendees.
	InvitationMail bool `json:"invitation_mail,omitempty"`
	OK             bool `json:"ok"`
}

func importanceName(n int) string {
	switch n {
	case 0:
		return "low"
	case 2:
		return "high"
	}
	return "normal"
}

// Raw is an answer that is a byte stream and not JSON: the message as a mail file. The command line writes Data to stdout and
// the REST API sends it with the content type, so both give the same bytes. An error is still the JSON error.
type Raw struct {
	Data        []byte
	ContentType string
	// Filename is a name for the file, without a path.
	Filename string
}

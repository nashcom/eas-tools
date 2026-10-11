package probe

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http/httptest"
	"net/mail"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/easmockserver"
)

func sessionCfg(url string, st *Store) Config {
	return Config{ServerURL: url, Username: easmockserver.User, Password: easmockserver.Password, Store: st}
}

func TestSessionSteps(t *testing.T) {
	fake := easmockserver.New()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	s, err := NewSession(sessionCfg(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()

	if err := s.Options(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if s.Server().ProtocolVersion != "14.1" {
		t.Errorf("version = %q", s.Server().ProtocolVersion)
	}
	if err := s.Provision(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if !s.Provisioned() {
		t.Error("device must be provisioned")
	}
	if s.HasDeviceInfo() {
		if err := s.DeviceInfo(ctx, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FolderSync(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if s.Inbox() != "5" {
		t.Errorf("inbox = %q", s.Inbox())
	}

	first, err := s.Sync(ctx, nil)
	if err != nil || !first.FirstSync {
		t.Fatalf("first sync: %+v, %v", first, err)
	}
	second, err := s.Sync(ctx, nil)
	if err != nil || second.FirstSync || second.Added != 1 {
		t.Fatalf("second sync: %+v, %v", second, err)
	}

	ping, err := s.Ping(ctx, 1, nil)
	if err != nil || ping.Changes {
		t.Fatalf("ping: %+v, %v", ping, err)
	}
}

func TestSessionWithoutFolderSyncAfterRestart(t *testing.T) {
	fake := easmockserver.New()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "state.json")
	ctx := t.Context()

	st := openStore(t, path)
	s1, _ := NewSession(sessionCfg(srv.URL, st))
	for _, step := range []func() error{
		func() error { return s1.Options(ctx, nil) },
		func() error { return s1.Provision(ctx, nil) },
		func() error { return s1.FolderSync(ctx, nil) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}

	// A restarted worker knows its Inbox from the stored hierarchy and can
	// sync at once, without Options/FolderSync.
	st2 := openStore(t, path)
	s2, _ := NewSession(sessionCfg(srv.URL, st2))
	if s2.Inbox() != "5" {
		t.Fatalf("inbox after restart = %q", s2.Inbox())
	}
	if _, err := s2.Sync(ctx, nil); err != nil {
		t.Fatalf("sync after restart: %v", err)
	}
}

func TestSessionHookAndMemory(t *testing.T) {
	fake := easmockserver.New()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	var n atomic.Int64
	cfg := sessionCfg(srv.URL, nil)
	cfg.OnExchange = func(Exchange) { n.Add(1) }
	s, _ := NewSession(cfg)
	defer s.Close()

	ctx := t.Context()
	_ = s.Options(ctx, nil)
	_ = s.Provision(ctx, nil)
	_ = s.FolderSync(ctx, nil)
	_, _ = s.Sync(ctx, nil)

	if n.Load() < 5 {
		t.Errorf("hook called %d times, want at least 5", n.Load())
	}
	if len(s.rec.all()) != 0 {
		t.Error("a session must not keep its exchanges in memory")
	}
}

func TestSessionSendsCookie(t *testing.T) {
	fake := easmockserver.New()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	s, _ := NewSession(sessionCfg(srv.URL, nil))
	defer s.Close()
	ctx := t.Context()
	_ = s.Options(ctx, nil)
	_ = s.Provision(ctx, nil)
	_ = s.FolderSync(ctx, nil)

	if fake.CookieHits() == 0 {
		t.Error("the session cookie from OPTIONS must be sent on later requests")
	}
}

func TestNewSessionNeedsServer(t *testing.T) {
	cfg := sessionCfg("", nil)
	cfg.Email = "alice@example.com"
	if _, err := NewSession(cfg); err == nil {
		t.Error("a session without server URL must be refused")
	}
}

func TestDeviceNumberGivesOwnDeviceID(t *testing.T) {
	srv := httptest.NewServer(easmockserver.New())
	defer srv.Close()

	id := func(n int) string {
		cfg := sessionCfg(srv.URL, nil)
		cfg.DeviceNumber = n
		s, err := NewSession(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		return s.DeviceID()
	}

	first, one, two, three := id(0), id(1), id(2), id(3)
	if first != one {
		t.Error("device number 0 and 1 must both be the first device")
	}
	if two == first || three == first || two == three {
		t.Errorf("each device number needs its own id: %s %s %s", first, two, three)
	}
}

func TestDirectoryFindsSelf(t *testing.T) {
	srv := httptest.NewServer(easmockserver.New())
	defer srv.Close()

	cfg := sessionCfg(srv.URL, nil)
	cfg.Username = "carol@example.com"
	s, err := NewSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	_ = s.Options(ctx, nil)
	_ = s.Provision(ctx, nil)

	res, err := s.Directory(ctx, nil, "carol@example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Entries < 1 || !res.SelfChecked || !res.SelfFound {
		t.Errorf("own user not found: %+v", res)
	}

	// An unknown address falls back to the part before the @, finds nothing.
	res, err = s.Directory(ctx, nil, "nobody-here@example.com", true)
	if err != nil || res.Entries != 0 || res.SelfFound {
		t.Errorf("unknown user: %+v, %v", res, err)
	}

	if _, err := s.Directory(ctx, nil, "  ", true); err == nil {
		t.Error("an empty query must be refused")
	}
}

func TestProbeDirectoryStep(t *testing.T) {
	srv := httptest.NewServer(easmockserver.New())
	defer srv.Close()

	cfg := baseCfg(srv.URL, nil)
	cfg.Username = "dave@example.com"
	res, err := Run(t.Context(), cfg)
	if err != nil || !res.OK {
		var buf bytes.Buffer
		WriteText(&buf, res)
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	d := res.Step(StepDirectory)
	if d == nil || !d.OK || d.Info["self_found"] != "true" {
		t.Errorf("directory step = %+v", d)
	}

	// off: the step is not there at all
	cfg.Directory = "off"
	res, _ = Run(t.Context(), cfg)
	if res.Step(StepDirectory) != nil {
		t.Error("directory off must not run the step")
	}

	// a query that matches nobody fails the step, not the rest of the run
	cfg.Directory = "on"
	cfg.DirectoryQuery = "zzz-unknown"
	res, _ = Run(t.Context(), cfg)
	if d := res.Step(StepDirectory); d == nil || d.OK {
		t.Errorf("empty directory result must fail the step: %+v", d)
	}
	if s := res.Step(StepSync); s == nil || !s.OK {
		t.Error("a failed directory step must not stop the other steps")
	}
}

func TestSendMailRoundTripAndDelete(t *testing.T) {
	fake := easmockserver.New()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	cfg := sessionCfg(srv.URL, nil)
	cfg.Username = "erin@example.com"
	s, err := NewSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	for _, step := range []func() error{
		func() error { return s.Options(ctx, nil) },
		func() error { return s.Provision(ctx, nil) },
		func() error { return s.FolderSync(ctx, nil) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Sync(ctx, nil); err != nil { // bootstrap
		t.Fatal(err)
	}

	m := Mail{To: []string{"erin@example.com"}, Subject: "[easload test #1]", Size: 3000, SkipSaveInSent: true}
	if err := s.SendMail(ctx, nil, m, "tag1"); err != nil {
		t.Fatal(err)
	}
	if fake.SentCount() != 1 || fake.MailboxCount("erin@example.com") != 1 {
		t.Fatalf("sent %d, mailbox %d", fake.SentCount(), fake.MailboxCount("erin@example.com"))
	}

	// the mail comes back through Sync, with its subject and server id
	var found MailRef
	for i := 0; i < 3 && found.ServerID == ""; i++ {
		res, err := s.Sync(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res.Mails {
			if r.Subject == "[easload test #1]" {
				found = r
			}
		}
	}
	if found.ServerID == "" {
		t.Fatal("the sent mail did not arrive in the Inbox")
	}

	if err := s.DeleteMail(ctx, nil, found.ServerID); err != nil {
		t.Fatal(err)
	}
	if fake.MailboxCount("erin@example.com") != 0 {
		t.Error("the mail must be gone after the delete")
	}

	// a mail to somebody who is not there is dropped; no recipient is refused
	if err := s.SendMail(ctx, nil, Mail{To: nil, Subject: "x"}, "t2"); err == nil {
		t.Error("a mail without recipient must be refused")
	}
	noAddr := sessionCfg(srv.URL, nil) // user name "alice": no address to send from
	s2, _ := NewSession(noAddr)
	defer s2.Close()
	_ = s2.Options(ctx, nil)
	_ = s2.Provision(ctx, nil)
	if err := s2.SendMail(ctx, nil, Mail{To: []string{"a@b.c"}, Subject: "x"}, "t3"); err == nil {
		t.Error("sending without a sender address must be refused")
	}
}

func TestBuildMIME(t *testing.T) {
	b := string(BuildMIME("a@example.com", Mail{To: []string{"b@example.com", "c@example.com"}, Subject: "hi", Size: 1000}, "TAG"))
	for _, want := range []string{"From: a@example.com\r\n", "To: b@example.com, c@example.com\r\n", "Subject: hi\r\n",
		"X-Easload-Tag: TAG\r\n", "Message-ID: <TAG@easload.invalid>\r\n", "\r\n\r\neasload test mail"} {
		if !strings.Contains(b, want) {
			t.Errorf("message lacks %q:\n%s", want, b)
		}
	}
	if len(b) < 1000 {
		t.Errorf("message is %d bytes, want at least the requested 1000 of body", len(b))
	}
}

// HCL Traveler refuses a delete Sync that lacks the Options of the Inbox Sync
// with status 3, whatever the sync key; the delete has to carry them.
func TestDeleteMailWorksOnAServerThatRequiresOptions(t *testing.T) {
	fake := easmockserver.NewWithOptions(easmockserver.Options{RequireOptions: true})
	srv := httptest.NewServer(fake)
	defer srv.Close()

	cfg := sessionCfg(srv.URL, nil)
	cfg.Username = "erin@example.com"
	s, err := NewSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	for _, step := range []func() error{
		func() error { return s.Options(ctx, nil) },
		func() error { return s.Provision(ctx, nil) },
		func() error { return s.FolderSync(ctx, nil) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Sync(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SendMail(ctx, nil, Mail{To: []string{"erin@example.com"}, Subject: "[opt]", Size: 500, SkipSaveInSent: true}, "t1"); err != nil {
		t.Fatal(err)
	}
	var found MailRef
	for i := 0; i < 3 && found.ServerID == ""; i++ {
		res, err := s.Sync(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res.Mails {
			if r.Subject == "[opt]" {
				found = r
			}
		}
	}
	if found.ServerID == "" {
		t.Fatal("the sent mail did not arrive")
	}
	if err := s.DeleteMail(ctx, nil, found.ServerID); err != nil {
		t.Fatal(err)
	}
	if fake.MailboxCount("erin@example.com") != 0 {
		t.Error("the mail must be gone after the delete")
	}
}

// The mail test only ever touches its own mail: other mail in the Inbox (more
// than one Sync window of it) is skipped and stays where it is.
func TestMailRoundTripLeavesOtherMailAlone(t *testing.T) {
	fake := easmockserver.New()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	cfg := sessionCfg(srv.URL, nil)
	cfg.Username = "erin@example.com"
	s, err := NewSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	for _, step := range []func() error{
		func() error { return s.Options(ctx, nil) },
		func() error { return s.Provision(ctx, nil) },
		func() error { return s.FolderSync(ctx, nil) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}

	const others = 130 // more than two windows of the mail test
	for i := 0; i < others; i++ {
		m := Mail{To: []string{"erin@example.com"}, Subject: fmt.Sprintf("other mail %d", i), Size: 100, SkipSaveInSent: true}
		if err := s.SendMail(ctx, nil, m, fmt.Sprintf("o%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	st := &Step{Info: map[string]string{}}
	trip, err := s.MailRoundTrip(ctx, st, MailTripOptions{Arrival: "poll", Wait: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !trip.Arrived || !trip.Deleted {
		t.Fatalf("the test mail must arrive and be deleted: %+v", trip)
	}
	if st.Info["subject"] != trip.Subject || trip.Subject == "" {
		t.Errorf("the step must name the subject of the test mail, got %q", st.Info["subject"])
	}
	if n := fake.MailboxCount("erin@example.com"); n != others {
		t.Errorf("%d other mails must remain, the mailbox has %d", others, n)
	}
}

func TestBuildMIMEWithBinaryAttachments(t *testing.T) {
	raw := BuildMIME("a@example.com", Mail{To: []string{"b@example.com"}, Subject: "hi", Size: 100, Attachments: 2, AttachSize: 5000, AttachBinary: true}, "TAG")

	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || "multipart/mixed" != mt {
		t.Fatalf("content type %q: %v", msg.Header.Get("Content-Type"), err)
	}

	mr := multipart.NewReader(msg.Body, params["boundary"])
	var parts, attachBytes int
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		parts++
		body, err := io.ReadAll(p)
		if err != nil {
			t.Fatal(err)
		}
		if 1 == parts {
			if !strings.Contains(string(body), "easload test mail") {
				t.Errorf("first part is not the text: %q", body)
			}
			continue
		}
		data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(body), "\r\n", ""))
		if err != nil {
			t.Fatalf("attachment %d: %v", parts-1, err)
		}
		attachBytes += len(data)
		if !strings.HasSuffix(p.FileName(), ".bin") {
			t.Errorf("file name %q", p.FileName())
		}
	}
	if 3 != parts || 10000 != attachBytes {
		t.Errorf("parts %d, attachment bytes %d", parts, attachBytes)
	}
}

func TestBuildMIMEWithTextAttachments(t *testing.T) {
	raw := BuildMIME("a@example.com", Mail{To: []string{"b@example.com"}, Subject: "hi", Size: 100, Attachments: 2, AttachSize: 3000}, "TAG-1")

	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}

	mr := multipart.NewReader(msg.Body, params["boundary"])
	var parts int
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		parts++
		body, err := io.ReadAll(p)
		if err != nil {
			t.Fatal(err)
		}
		if 1 == parts {
			continue
		}
		if want := fmt.Sprintf("attachment-%d.txt", parts-1); want != p.FileName() {
			t.Errorf("file name %q, want %q", p.FileName(), want)
		}
		if ct := p.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("content type %q", ct)
		}
		text := string(body)
		// the part ends with the line break before the next boundary
		text = strings.TrimSuffix(text, "\r\n")
		if 3000 != len(text) {
			t.Errorf("attachment %d is %d bytes, want 3000", parts-1, len(text))
		}
		if !strings.HasPrefix(text, "easload test attachment\r\ntag:  TAG-1\r\nfile: ") {
			t.Errorf("attachment %d starts with %q", parts-1, text[:60])
		}
		if strings.Contains(text, "\r\n--") {
			t.Errorf("attachment %d contains a line that looks like a boundary", parts-1)
		}
	}
	if 3 != parts {
		t.Errorf("parts %d, want 3", parts)
	}
}

func TestMailWatchTimeline(t *testing.T) {
	w := &mailWatch{start: time.Now()}
	quiet := &SyncResult{SyncKey: "7"}
	for i := 0; i < 20; i++ {
		w.sync(quiet, 50*time.Millisecond, "me")
	}
	// the first 3 and the 15th of the quiet Syncs are noted, the others are only counted
	if 20 != w.polls || 4 != len(w.events) {
		t.Fatalf("polls %d, events %d: %v", w.polls, len(w.events), w.events)
	}

	busy := &SyncResult{Added: 2, SyncKey: "8", Mails: []MailRef{{Subject: "other"}, {Subject: "me"}}}
	w.sync(busy, 90*time.Millisecond, "me")
	last := w.events[len(w.events)-1]
	if !strings.Contains(last, "added=2") || !strings.Contains(last, "other; me <- this mail") {
		t.Errorf("last event %q", last)
	}
	if 2 != w.seen || "8" != w.key {
		t.Errorf("seen %d key %q", w.seen, w.key)
	}

	for i := 0; i < 2*maxWatchEvents; i++ {
		w.note("x")
	}
	if maxWatchEvents != len(w.events) || 0 == w.cut {
		t.Errorf("events %d, cut %d: the timeline must stay bounded", len(w.events), w.cut)
	}
}

// A server that keeps a copy of every sent mail in Sent Items although none was asked for: DeleteSentCopies removes
// the copies of the given subjects and leaves every other mail of the folder alone.
func TestDeleteSentCopiesOnlyDeletesOurMail(t *testing.T) {
	fake := easmockserver.NewWithOptions(easmockserver.Options{KeepSentCopy: true})
	srv := httptest.NewServer(fake)
	defer srv.Close()

	cfg := sessionCfg(srv.URL, nil)
	cfg.Username = "erin@example.com"
	s, err := NewSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	for _, step := range []func() error{
		func() error { return s.Options(ctx, nil) },
		func() error { return s.Provision(ctx, nil) },
		func() error { return s.FolderSync(ctx, nil) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if "" == s.SentID() {
		t.Fatal("the server offers a Sent Items folder")
	}

	ours := map[string]bool{}
	for i := 0; i < 3; i++ {
		subject := fmt.Sprintf("[easload test #%d]", i)
		ours[subject] = true
		if err := s.SendMail(ctx, nil, Mail{To: []string{"erin@example.com"}, Subject: subject, Size: 100, SkipSaveInSent: true}, fmt.Sprintf("t%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SendMail(ctx, nil, Mail{To: []string{"erin@example.com"}, Subject: "somebody else's mail", Size: 100, SkipSaveInSent: true}, "other"); err != nil {
		t.Fatal(err)
	}
	if 4 != fake.SentboxCount("erin@example.com") {
		t.Fatalf("the server must keep the copies: %d", fake.SentboxCount("erin@example.com"))
	}

	deleted, failed, err := s.DeleteSentCopies(ctx, ours)
	if err != nil || 3 != deleted || 0 != failed {
		t.Fatalf("deleted %d, failed %d, %v", deleted, failed, err)
	}
	if 1 != fake.SentboxCount("erin@example.com") {
		t.Errorf("only the mail that is not ours must be left: %d", fake.SentboxCount("erin@example.com"))
	}

	// a second run finds nothing
	if deleted, _, err := s.DeleteSentCopies(ctx, ours); err != nil || 0 != deleted {
		t.Errorf("second run: deleted %d, %v", deleted, err)
	}
}

// With an extra wait, a mail that comes after the wait time is late (and deleted); without it, the same mail is lost.
func TestMailRoundTripLateAndLost(t *testing.T) {
	for _, tc := range []struct {
		name      string
		extra     time.Duration
		want      error
		wantInbox int
	}{
		{"extra wait: late", 3 * time.Second, ErrMailLate, 0},
		{"no extra wait: lost", 0, ErrMailLost, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := easmockserver.NewWithOptions(easmockserver.Options{MailDelay: 1500 * time.Millisecond})
			srv := httptest.NewServer(fake)
			defer srv.Close()

			cfg := sessionCfg(srv.URL, nil)
			cfg.Username = "erin@example.com"
			s, err := NewSession(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			ctx := t.Context()
			for _, step := range []func() error{
				func() error { return s.Options(ctx, nil) },
				func() error { return s.Provision(ctx, nil) },
				func() error { return s.FolderSync(ctx, nil) },
			} {
				if err := step(); err != nil {
					t.Fatal(err)
				}
			}

			trip, err := s.MailRoundTrip(ctx, nil, MailTripOptions{Wait: time.Second, ExtraWait: tc.extra, Arrival: "poll", SizeKB: 1})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error %v, want %v", err, tc.want)
			}
			if trip == nil || !trip.Sent || trip.Arrived || (ErrMailLate == tc.want) != trip.Late {
				t.Errorf("trip %+v", trip)
			}
			if ErrMailLate == tc.want && (!trip.Deleted || trip.LateAfter < time.Second) {
				t.Errorf("a late mail is found after the wait and deleted: %+v", trip)
			}
			// the delayed mail reaches the mailbox in the end; it is only gone when it was found and deleted
			time.Sleep(2 * time.Second)
			if n := fake.MailboxCount("erin@example.com"); tc.wantInbox != n {
				t.Errorf("mailbox has %d mails, want %d", n, tc.wantInbox)
			}
		})
	}
}

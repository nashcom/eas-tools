// eas-mock-server is a mock ActiveSync server for demos and tests of the
// exporter and the load worker. It is NOT a mail server: it answers the
// ActiveSync commands with a tiny made-up mailbox (an Inbox that gets new
// mail over time) and can add latency and errors, so that the monitoring
// and load tools can be tried without a real Traveler.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nashcom/eas-tools/internal/easmockserver"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		listen    = flag.String("listen", "127.0.0.1:8099", "listen address (plain HTTP; put TLS in front if needed)")
		password  = flag.String("password", easmockserver.Password, "password every account uses (any user name is accepted)")
		latency   = flag.Duration("latency", 0, "delay added to every answer, e.g. 50ms")
		errorRate = flag.Float64("error-rate", 0, "share of requests answered with HTTP 503, 0..1")
		mailEvery = flag.Duration("mail-every", 0, "a new mail arrives for every device each interval, e.g. 30s (default: one mail per sync)")
		holdPing  = flag.Bool("hold-ping", true, "hold Ping requests for the heartbeat like a push server (wakes on new mail)")
		dropMail  = flag.Bool("drop-mail", false, "accept SendMail but never deliver the mail (to see a lost mail)")
		mailbox   = flag.Bool("mailbox", false, "a small real mailbox: folders, mails that stay until moved or deleted, Fetch, MoveItems, SendMail with a copy in Sent Items (for eas-client; mails sent to a user that has logged in arrive)")
		sim       = flag.Bool("sim", false, "honor the X-Mock-Sim request header: the request tells the server how to misbehave (delay, http status, EAS status, garbage, close; see docs/MOCKSERVER.md)")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "eas-mock-server - mock ActiveSync server for demos\n\nUsage:\n  eas-mock-server -listen :8099 -mail-every 30s -latency 50ms\n\nOptions:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *errorRate < 0 || *errorRate > 1 {
		fmt.Fprintln(os.Stderr, "eas-mock-server: -error-rate must be between 0 and 1")
		return 2
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := &http.Server{
		Addr: *listen,
		Handler: easmockserver.NewWithOptions(easmockserver.Options{
			Password:  *password,
			Latency:   *latency,
			ErrorRate: *errorRate,
			MailEvery: *mailEvery,
			HoldPing:  *holdPing,
			DropMail:  *dropMail,
			Sim:       *sim,
			Mailbox:   *mailbox,
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("started", "listen", *listen, "latency", *latency, "error_rate", *errorRate,
		"mail_every", *mailEvery, "hold_ping", *holdPing, "endpoint", "/Microsoft-Server-ActiveSync")

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("listener", "error", err)
			return 1
		}
	case <-ctx.Done():
		log.Info("shutting down")
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	return 0
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"slopbooru/imgrec/internal/api"
	"slopbooru/imgrec/internal/auth"
	"slopbooru/imgrec/internal/pipeline"
	"slopbooru/imgrec/internal/store"
)

// imgrec serve: the HTTP API, with the scheduler running behind it.
func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var f fleet
	f.flags(fs)
	listen := fs.String("listen", env("LISTEN", "127.0.0.1:8100"), "address to listen on")
	maxUpload := fs.Int64("max-upload", 256<<20, "largest picture accepted, in bytes")
	maxWait := fs.Duration("max-wait", 5*time.Minute, "cap on a request's ?wait=")
	tlsCert := fs.String("tls-cert", env("TLS_CERT", ""), "serve https with this certificate (PEM; re-read when it changes)")
	tlsKey := fs.String("tls-key", env("TLS_KEY", ""), "the certificate's private key (PEM)")
	fs.Parse(args)
	if (*tlsCert == "") != (*tlsKey == "") {
		return fmt.Errorf("--tls-cert and --tls-key go together")
	}

	p, sc, closeAll, err := f.open()
	if err != nil {
		return err
	}
	defer closeAll()
	a, err := auth.Open(p.Store.DB)
	if err != nil {
		return err
	}
	if cs, _ := a.List(); len(cs) == 0 {
		fmt.Fprintln(os.Stderr, "imgrec: no clients yet -- every request will be refused (imgrec client add NAME)")
	}
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "imgrec: "+format+"\n", a...) }
	srv := &http.Server{
		Handler:           &api.Server{P: p, Auth: a, Sched: sc, MaxUpload: *maxUpload, MaxWait: *maxWait, Logf: logf},
		ReadHeaderTimeout: 30 * time.Second,
	}
	scheme := "http"
	if *tlsCert != "" {
		certs, err := newCertReloader(*tlsCert, *tlsKey, logf)
		if err != nil {
			return err
		}
		srv.TLSConfig = certs.config()
		scheme = "https"
		c, _ := certs.get(nil)
		logf("tls: %s (%s)", *tlsCert, fingerprint(c.Certificate[0]))
	} else if cleartextRemote("http://" + *listen) {
		logf("warning: no --tls-cert, so tokens and pictures cross the network in the clear")
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	schedDone := make(chan error, 1)
	go func() { schedDone <- sc.Run(ctx, false) }()
	// uploads no job needs any more are deleted (the answers stay); the
	// grace keeps a fresh upload alive until its jobs are queued
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := p.Sweep(time.Minute); err != nil {
					logf("sweep: %v", err)
				} else if n > 0 {
					logf("deleted %d uploaded picture(s) no job needs", n)
				}
			}
		}
	}()
	go func() {
		<-ctx.Done()
		shut, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		srv.Shutdown(shut)
	}()
	logf("listening on %s://%s", scheme, ln.Addr())
	serve := func() error { return srv.Serve(ln) }
	if srv.TLSConfig != nil {
		serve = func() error { return srv.ServeTLS(ln, "", "") }
	}
	if err := serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logf("stopping: runners are being shut down")
	if err := <-schedDone; err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// imgrec client add|list|remove: who may use the API.
func clientCmd(args []string) error {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	db := fs.String("db", env("DB", "results.db"), "the db the server uses")
	admin := fs.Bool("admin", false, "with add: may see every client's pictures")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: imgrec client [--db DB] add NAME [--admin] | list | remove NAME")
	}
	fs.Parse(args)
	rest := fs.Args()
	if len(rest) > 2 { // flags may come after the name too
		fs.Parse(rest[2:])
		rest = rest[:2]
	}
	if len(rest) == 0 {
		fs.Usage()
		os.Exit(2)
	}
	st, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := pipeline.Schema(st.DB); err != nil { // client remove clears their jobs
		return err
	}
	a, err := auth.Open(st.DB)
	if err != nil {
		return err
	}
	switch {
	case rest[0] == "add" && len(rest) == 2:
		token, err := a.Add(rest[1], *admin)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "imgrec: client %q added%s. Its token (shown once, keep it):\n",
			rest[1], map[bool]string{true: " (admin)"}[*admin])
		fmt.Println(token)
	case rest[0] == "list" && len(rest) == 1:
		cs, err := a.List()
		if err != nil {
			return err
		}
		for _, c := range cs {
			role := ""
			if c.Admin {
				role = " admin"
			}
			fmt.Printf("%s\t%s%s\n", c.Name, c.Created, role)
		}
	case rest[0] == "remove" && len(rest) == 2:
		return a.Remove(rest[1])
	default:
		fs.Usage()
		os.Exit(2)
	}
	return nil
}

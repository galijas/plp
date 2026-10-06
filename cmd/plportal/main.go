// Command plportal is the Private Label Portal server and its admin CLI.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"plportal/internal/auth"
	"plportal/internal/server"
	"plportal/internal/store"
)

var version = "dev"

const usage = `Private Label Portal %s

Usage:
  plportal serve -domain <dns name> -email <address> [-data-dir DIR] [-timezone ZONE]
  plportal serve -dev-addr 127.0.0.1:8080 [-data-dir DIR]          (plain HTTP, local testing only)
  plportal create-admin -username NAME [-email ADDR] [-data-dir DIR] (prints a generated password)
  plportal reset-password -username NAME [-data-dir DIR]             (prints a generated password)
  plportal backup [-data-dir DIR] [-keep N]                          (database only; see README for files)
  plportal version
`

const defaultDataDir = "/var/lib/plportal"

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "create-admin":
		err = cmdAdmin(os.Args[2:], false)
	case "reset-password":
		err = cmdAdmin(os.Args[2:], true)
	case "backup":
		err = cmdBackup(os.Args[2:])
	case "version", "-version", "--version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("plportal: %v", err)
	}
}

func openStore(dataDir string) (*store.Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	return store.Open(filepath.Join(dataDir, "plportal.db"))
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir, "directory for the database, files and certificates")
	domain := fs.String("domain", "", "public DNS name of this server (Let's Encrypt certificate)")
	email := fs.String("email", "", "contact email for Let's Encrypt")
	httpsAddr := fs.String("https-addr", ":443", "HTTPS listen address")
	httpAddr := fs.String("http-addr", ":80", "HTTP listen address (certificate challenges and redirect to HTTPS)")
	devAddr := fs.String("dev-addr", "", "serve plain HTTP on this address instead, without TLS (local testing only)")
	tz := fs.String("timezone", "Europe/Sarajevo", "time zone for displayed times and folder names")
	maxTotal := fs.Int64("max-submission-mb", 3072, "maximum size of all files in one submission, in MB")
	fs.Parse(args)

	if *devAddr == "" && (*domain == "" || *email == "") {
		return errors.New("serve needs -domain and -email (or -dev-addr for local testing)")
	}
	loc, err := time.LoadLocation(*tz)
	if err != nil {
		return fmt.Errorf("time zone %q: %w", *tz, err)
	}
	logger := log.New(os.Stderr, "", 0)
	if os.Getenv("INVOCATION_ID") == "" { // not under systemd, which timestamps itself
		logger.SetFlags(log.LstdFlags)
	}

	st, err := openStore(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	host := strings.ToLower(strings.TrimSuffix(*domain, "."))
	public := "https://" + host
	if *devAddr != "" {
		public = "http://" + *devAddr
	}
	srv, err := server.New(server.Config{
		Store: st, DataDir: *dataDir, PublicURL: public, Secure: *devAddr == "",
		MaxSubmissionBytes: *maxTotal << 20, Location: loc, Version: version, Logger: logger,
	})
	if err != nil {
		return err
	}
	stop := make(chan struct{})
	go srv.Maintenance(stop)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var servers []*http.Server
	errc := make(chan error, 2)
	if *devAddr != "" {
		logger.Printf("WARNING: serving plain HTTP on %s (dev mode; never expose this)", *devAddr)
		hs := newHTTPServer(*devAddr, srv.Handler(), logger)
		servers = append(servers, hs)
		go func() { errc <- hs.ListenAndServe() }()
	} else {
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(host),
			Cache:      autocert.DirCache(filepath.Join(*dataDir, "autocert")),
			Email:      *email,
		}
		tlsCfg := m.TLSConfig()
		tlsCfg.MinVersion = tls.VersionTLS12
		hs := newHTTPServer(*httpsAddr, srv.Handler(), logger)
		hs.TLSConfig = tlsCfg
		redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
		})
		plain := newHTTPServer(*httpAddr, m.HTTPHandler(redirect), logger)
		plain.ReadTimeout, plain.WriteTimeout = 10*time.Second, 10*time.Second
		servers = append(servers, hs, plain)
		logger.Printf("serving https://%s/ on %s, HTTP on %s", host, *httpsAddr, *httpAddr)
		go func() { errc <- hs.ListenAndServeTLS("", "") }()
		go func() { errc <- plain.ListenAndServe() }()
	}

	select {
	case <-ctx.Done():
		logger.Printf("shutting down")
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			close(stop)
			return err
		}
	}
	close(stop)
	sctx, scancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer scancel()
	for _, hs := range servers {
		hs.Shutdown(sctx)
	}
	return nil
}

// Upload chunks are at most 16 MB, so a 5-minute read timeout allows slow
// links (about 450 kbit/s) while still dropping stalled connections.
// Downloads of large zips have no write timeout.
func newHTTPServer(addr string, h http.Handler, logger *log.Logger) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          log.New(filteredWriter{logger}, "", 0),
	}
}

// filteredWriter drops TLS handshake noise from internet scanners.
type filteredWriter struct{ l *log.Logger }

func (f filteredWriter) Write(p []byte) (int, error) {
	s := string(p)
	if strings.Contains(s, "TLS handshake error") {
		return len(p), nil
	}
	f.l.Print(strings.TrimRight(s, "\n"))
	return len(p), nil
}

func cmdAdmin(args []string, reset bool) error {
	name := "create-admin"
	if reset {
		name = "reset-password"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir, "data directory")
	username := fs.String("username", "", "admin username")
	email := fs.String("email", "", "notification email (create-admin only)")
	fs.Parse(args)
	if *username == "" {
		return errors.New("-username is required")
	}
	st, err := openStore(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	pw := auth.GeneratePassword()
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	if reset {
		a, err := st.AdminByUsername(*username)
		if err != nil {
			return fmt.Errorf("no admin named %q", *username)
		}
		if err := st.SetAdminPassword(a.ID, hash, ""); err != nil {
			return err
		}
		st.Log(store.ActorSystem, "cli", "", "account.reset_password", a.Username+" (command line)")
	} else {
		if _, err := st.CreateAdmin(*username, strings.ToLower(strings.TrimSpace(*email)), hash, "cli"); err != nil {
			if errors.Is(err, store.ErrExists) {
				return fmt.Errorf("an admin named %q already exists (use reset-password)", *username)
			}
			return err
		}
		st.Log(store.ActorSystem, "cli", "", "account.create", *username+" (command line)")
	}
	fmt.Printf("Admin username: %s\nAdmin password: %s\n", *username, pw)
	return nil
}

func cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir, "data directory")
	keep := fs.Int("keep", 14, "number of backups to keep")
	fs.Parse(args)
	st, err := openStore(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	dir := filepath.Join(*dataDir, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	dest := filepath.Join(dir, "plportal-"+time.Now().UTC().Format("20060102-150405")+".db")
	if err := st.Backup(dest); err != nil {
		return err
	}
	fmt.Println("backup written to", dest)
	matches, _ := filepath.Glob(filepath.Join(dir, "plportal-*.db"))
	sort.Strings(matches)
	for len(matches) > *keep {
		os.Remove(matches[0])
		matches = matches[1:]
	}
	return nil
}

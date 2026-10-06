// Package server serves the portal: the login page, the submission form
// (guests fill it in, admins preview and edit it), the admin panel and the
// submission explorer.
//
// Guests sign in with an email and an invite code; that session can only
// read the form, save its own draft, upload files to its own draft and
// submit. Everything else needs an admin session.
package server

import (
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // display time zone without relying on the host's zoneinfo

	"plportal/internal/store"
)

//go:embed web
var webFS embed.FS

const (
	adminSessionTTL = 12 * time.Hour
	guestSessionTTL = 24 * time.Hour
)

type Config struct {
	Store   *store.Store
	DataDir string
	// PublicURL is used in invite links and notification emails, e.g. https://forms.example.com
	PublicURL string
	// Secure is true when served over HTTPS: Secure/__Host- cookies and HSTS.
	Secure bool
	// MaxSubmissionBytes caps the total size of one guest's staged uploads.
	MaxSubmissionBytes int64
	Location           *time.Location
	Version            string
	Logger             *log.Logger
}

type Server struct {
	cfg      Config
	st       *store.Store
	log      *log.Logger
	loc      *time.Location
	pages    map[string]*template.Template
	assetVer string
	logins   *limiter
	handler  http.Handler

	uploadLocks sync.Map // upload ID -> *sync.Mutex
	submitLocks sync.Map // invite ID + email -> *sync.Mutex
}

func New(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	if cfg.MaxSubmissionBytes <= 0 {
		cfg.MaxSubmissionBytes = 3 << 30
	}
	cfg.PublicURL = strings.TrimSuffix(cfg.PublicURL, "/")
	s := &Server{cfg: cfg, st: cfg.Store, log: cfg.Logger, loc: cfg.Location, logins: newLimiter(10, 15*time.Minute)}
	for _, d := range []string{s.stagingDir(), s.submissionsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	if err := s.loadTemplates(); err != nil {
		return nil, err
	}
	if _, _, err := s.st.CurrentForm(); err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web/static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler(http.FileServerFS(static))))
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Header().Set("Content-Type", "image/svg+xml")
		b, _ := webFS.ReadFile("web/static/icon.svg")
		w.Write(b)
	})

	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("POST /login/guest", s.handleGuestLogin)
	mux.HandleFunc("POST /login/admin", s.handleAdminLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("GET /form", s.anyUser(s.handleFormPage))
	mux.HandleFunc("GET /done", s.handleDone)

	mux.HandleFunc("GET /api/form", s.anyUser(s.apiForm))
	mux.HandleFunc("PUT /api/draft", s.guest(s.apiSaveDraft))
	mux.HandleFunc("POST /api/uploads", s.guest(s.apiUploadCreate))
	mux.HandleFunc("PUT /api/uploads/{id}", s.guest(s.apiUploadChunk))
	mux.HandleFunc("DELETE /api/uploads/{id}", s.guest(s.apiUploadDelete))
	mux.HandleFunc("POST /api/submit", s.guest(s.apiSubmit))
	mux.HandleFunc("PUT /api/admin/form", s.admin(s.apiSaveForm))

	mux.HandleFunc("GET /admin", s.admin(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/submissions", http.StatusSeeOther)
	}))
	mux.HandleFunc("GET /admin/submissions", s.admin(s.handlePanelSubmissions))
	mux.HandleFunc("GET /admin/invites", s.admin(s.handleInvites))
	mux.HandleFunc("POST /admin/invites", s.admin(s.handleInviteCreate))
	mux.HandleFunc("POST /admin/invites/{id}", s.admin(s.handleInviteUpdate))
	mux.HandleFunc("POST /admin/invites/{id}/revoke", s.admin(s.handleInviteRevoke))
	mux.HandleFunc("POST /admin/invites/{id}/restore", s.admin(s.handleInviteRestore))
	mux.HandleFunc("POST /admin/invites/{id}/delete", s.admin(s.handleInviteDelete))
	mux.HandleFunc("GET /admin/logs", s.admin(s.handleLogs))
	mux.HandleFunc("GET /admin/accounts", s.admin(s.handleAccounts))
	mux.HandleFunc("POST /admin/accounts", s.admin(s.handleAccountCreate))
	mux.HandleFunc("POST /admin/accounts/{id}/reset", s.admin(s.handleAccountReset))
	mux.HandleFunc("POST /admin/accounts/{id}/email", s.admin(s.handleAccountEmail))
	mux.HandleFunc("POST /admin/accounts/{id}/delete", s.admin(s.handleAccountDelete))
	mux.HandleFunc("GET /admin/security", s.admin(s.handleSecurity))
	mux.HandleFunc("POST /admin/security/password", s.admin(s.handlePasswordChange))
	mux.HandleFunc("POST /admin/security/email", s.admin(s.handleOwnEmail))
	mux.HandleFunc("GET /admin/smtp", s.admin(s.handleSMTP))
	mux.HandleFunc("POST /admin/smtp", s.admin(s.handleSMTPSave))
	mux.HandleFunc("POST /admin/smtp/test", s.admin(s.handleSMTPTest))

	mux.HandleFunc("GET /explorer", s.admin(s.handleExplorer))
	mux.HandleFunc("GET /explorer/{id}", s.admin(s.handleFolder))
	mux.HandleFunc("GET /explorer/{id}/answers.txt", s.admin(s.handleAnswersText))
	mux.HandleFunc("GET /explorer/{id}/answers.pdf", s.admin(s.handleAnswersPDF))
	mux.HandleFunc("GET /explorer/{id}/files.zip", s.admin(s.handleFilesZip))
	mux.HandleFunc("POST /explorer/{id}/delete", s.admin(s.handleSubmissionDelete))

	s.handler = s.securityHeaders(http.NewCrossOriginProtection().Handler(mux))
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) stagingDir() string     { return filepath.Join(s.cfg.DataDir, "staging") }
func (s *Server) submissionsDir() string { return filepath.Join(s.cfg.DataDir, "submissions") }

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if s.cfg.Secure {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func staticHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// Maintenance runs periodic cleanup until stop is closed: expired sessions,
// abandoned partial uploads, and staging files without a record.
func (s *Server) Maintenance(stop <-chan struct{}) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	s.cleanup()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.cleanup()
		}
	}
}

func (s *Server) cleanup() {
	if err := s.st.PurgeExpiredSessions(); err != nil {
		s.log.Printf("purge sessions: %v", err)
	}
	s.logins.purge()
	stale, err := s.st.StaleUploads(time.Now().Add(-48 * time.Hour))
	if err != nil {
		s.log.Printf("stale uploads: %v", err)
	}
	for _, id := range stale {
		s.st.DeleteUpload(id)
		os.Remove(s.stagingPath(id))
	}
	known, err := s.st.AllUploadIDs()
	if err != nil {
		return
	}
	entries, _ := os.ReadDir(s.stagingDir())
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || known[e.Name()] || time.Since(info.ModTime()) < time.Hour {
			continue
		}
		os.Remove(filepath.Join(s.stagingDir(), e.Name()))
	}
}

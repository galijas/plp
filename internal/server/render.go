package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"plportal/internal/export"
	"plportal/internal/store"
)

// page is what every template gets.
type page struct {
	Title      string
	Tab        string // "form" or "panel" for admins; "" on guest and login pages
	Section    string // admin panel section
	Admin      *store.Admin
	GuestEmail string
	Flash      string
	FlashError bool
	Asset      string
	Version    string
	PublicURL  string
	Data       any
}

func (s *Server) loadTemplates() error {
	funcs := template.FuncMap{
		"when": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.In(s.loc).Format("2006-01-02 15:04")
		},
		"whenp": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.In(s.loc).Format("2006-01-02 15:04")
		},
		"whenFull": func(t time.Time) string { return t.In(s.loc).Format("2006-01-02 15:04:05 MST") },
		"bytes":    export.HumanBytes,
		"inviteLink": func(code, email string) string {
			v := url.Values{"invite": {code}}
			if email != "" {
				v.Set("email", email)
			}
			return s.publicURL() + "/#" + v.Encode()
		},
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
	}
	s.pages = map[string]*template.Template{}
	names, err := fs.Glob(webFS, "web/templates/*.html")
	if err != nil {
		return err
	}
	h := sha256.New()
	fs.WalkDir(webFS, "web/static", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := webFS.ReadFile(p)
			h.Write(b)
		}
		return nil
	})
	s.assetVer = hex.EncodeToString(h.Sum(nil))[:10]
	for _, n := range names {
		base := n[strings.LastIndex(n, "/")+1:]
		if base == "base.html" {
			continue
		}
		t, err := template.New(base).Funcs(funcs).ParseFS(webFS, "web/templates/base.html", n)
		if err != nil {
			return fmt.Errorf("template %s: %w", base, err)
		}
		s.pages[base] = t
	}
	return nil
}

func (s *Server) publicURL() string { return s.cfg.PublicURL }

func (s *Server) render(w http.ResponseWriter, r *http.Request, code int, name string, p page) {
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	p.Asset, p.Version, p.PublicURL = s.assetVer, s.cfg.Version, s.publicURL()
	if p.Admin == nil {
		p.Admin = adminFrom(r)
	}
	if p.Flash == "" {
		p.Flash, p.FlashError = s.takeFlash(w, r)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := t.ExecuteTemplate(w, "base", p); err != nil {
		s.log.Printf("render %s: %v", name, err)
	}
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, code int, msg string) {
	s.render(w, r, code, "error.html", page{Title: http.StatusText(code), Data: msg})
}

// Flash messages survive one redirect in a short-lived cookie. A leading
// "!" marks an error.
const flashCookie = "plp_flash"

func (s *Server) flash(w http.ResponseWriter, msg string) {
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: url.QueryEscape(msg), Path: "/", MaxAge: 60,
		HttpOnly: true, Secure: s.cfg.Secure, SameSite: http.SameSiteStrictMode})
}

func (s *Server) flashError(w http.ResponseWriter, msg string) { s.flash(w, "!"+msg) }

func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) (string, bool) {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return "", false
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.Secure, SameSite: http.SameSiteStrictMode})
	msg, _ := url.QueryUnescape(c.Value)
	if strings.HasPrefix(msg, "!") {
		return msg[1:], true
	}
	return msg, false
}

func (s *Server) redirectFlash(w http.ResponseWriter, r *http.Request, to, msg string) {
	s.flash(w, msg)
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, to, msg string) {
	s.flashError(w, msg)
	http.Redirect(w, r, to, http.StatusSeeOther)
}

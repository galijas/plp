package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"

	"plportal/internal/auth"
	"plportal/internal/store"
)

type ctxKey int

const (
	adminCtx ctxKey = iota
	guestCtx
)

// guestCtxValue is a guest's session with its invite, checked on every request.
type guestCtxValue struct {
	Email  string
	Invite *store.Invite
}

func (s *Server) cookieName() string {
	if s.cfg.Secure {
		return "__Host-plp_session"
	}
	return "plp_session"
}

func (s *Server) sessionFrom(r *http.Request) (*store.Session, string) {
	c, err := r.Cookie(s.cookieName())
	if err != nil || c.Value == "" {
		return nil, ""
	}
	h := auth.HashToken(c.Value)
	sess, err := s.st.SessionByToken(h)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Printf("session lookup: %v", err)
		}
		return nil, ""
	}
	return sess, h
}

// identify resolves the request's session into an admin or a guest (or neither).
// A guest session only counts while its invite code is still usable.
func (s *Server) identify(r *http.Request) (*store.Admin, *guestCtxValue) {
	sess, _ := s.sessionFrom(r)
	if sess == nil {
		return nil, nil
	}
	switch sess.Kind {
	case store.SessionAdmin:
		a, err := s.st.AdminByID(sess.AdminID)
		if err == nil {
			return a, nil
		}
	case store.SessionGuest:
		inv, err := s.st.InviteByID(sess.InviteID)
		if err == nil && inv.Status() == store.InviteActive && inv.AllowsEmail(sess.Email) {
			return nil, &guestCtxValue{Email: sess.Email, Invite: inv}
		}
	}
	return nil, nil
}

func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, _ := s.identify(r)
		if a == nil {
			if r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/") {
				http.Redirect(w, r, "/#admin", http.StatusSeeOther)
			} else {
				jsonError(w, http.StatusUnauthorized, "Your admin session has ended. Log in again.")
			}
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, r.WithContext(context.WithValue(r.Context(), adminCtx, a)))
	}
}

func (s *Server) guest(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, g := s.identify(r)
		if g == nil {
			jsonError(w, http.StatusUnauthorized, "Your session has ended or the invite code is no longer valid. Sign in again to continue; your draft is kept.")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, r.WithContext(context.WithValue(r.Context(), guestCtx, g)))
	}
}

// anyUser admits admins and guests; the handler checks which one it got.
func (s *Server) anyUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, g := s.identify(r)
		if a == nil && g == nil {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				jsonError(w, http.StatusUnauthorized, "Your session has ended. Sign in again to continue.")
			} else {
				http.Redirect(w, r, "/", http.StatusSeeOther)
			}
			return
		}
		ctx := r.Context()
		if a != nil {
			ctx = context.WithValue(ctx, adminCtx, a)
		} else {
			ctx = context.WithValue(ctx, guestCtx, g)
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, r.WithContext(ctx))
	}
}

func adminFrom(r *http.Request) *store.Admin {
	a, _ := r.Context().Value(adminCtx).(*store.Admin)
	return a
}

func guestFrom(r *http.Request) *guestCtxValue {
	g, _ := r.Context().Value(guestCtx).(*guestCtxValue)
	return g
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if a, g := s.identify(r); a != nil || g != nil {
		http.Redirect(w, r, "/form", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login.html", page{Title: "Sign in", Data: loginView{Mode: "guest"}})
}

type loginView struct {
	Mode     string // "guest" or "admin"
	Error    string
	Email    string
	Code     string
	Username string
}

func (s *Server) loginFail(w http.ResponseWriter, r *http.Request, code int, v loginView) {
	s.render(w, r, code, "login.html", page{Title: "Sign in", Data: v})
}

func (s *Server) startSession(w http.ResponseWriter, sess store.Session, ttl time.Duration) error {
	token := auth.NewSessionToken()
	if err := s.st.CreateSession(auth.HashToken(token), sess, ttl); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: token, Path: "/",
		MaxAge: int(ttl.Seconds()), HttpOnly: true, Secure: s.cfg.Secure, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func normalizeEmail(e string) (string, bool) {
	e = strings.ToLower(strings.TrimSpace(e))
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || len(e) > 254 || !strings.Contains(e[strings.LastIndex(e, "@"):], ".") {
		return e, false
	}
	return e, true
}

func (s *Server) handleGuestLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	email, okEmail := normalizeEmail(r.PostFormValue("email"))
	code := auth.NormalizeCode(r.PostFormValue("code"))
	v := loginView{Mode: "guest", Email: email, Code: code}
	if !s.logins.allowed(ip) {
		s.st.Log(store.ActorGuest, email, ip, "guest.login_blocked", "too many failed attempts")
		v.Error = "Too many failed attempts from your network. Wait 15 minutes, then try again."
		s.loginFail(w, r, http.StatusTooManyRequests, v)
		return
	}
	if !okEmail {
		v.Error = "Enter a valid email address."
		s.loginFail(w, r, http.StatusBadRequest, v)
		return
	}
	inv, err := s.st.InviteByCode(code)
	reason := ""
	switch {
	case code == "":
		reason = "no code"
	case err != nil:
		reason = "unknown code"
	case inv.Status() != store.InviteActive:
		reason = "code " + inv.Status()
	case !inv.AllowsEmail(email):
		reason = "code is for a different email"
	}
	if reason != "" {
		s.logins.failed(ip)
		s.st.Log(store.ActorGuest, email, ip, "guest.login_failed", reason+" ("+code+")")
		// One message for every reason, so the page doesn't reveal which
		// codes exist or which email a code belongs to.
		v.Error = "This email and invite code don't match a valid invitation. Check both against the invitation you received, or ask your Bicom Systems contact for a new code."
		s.loginFail(w, r, http.StatusUnauthorized, v)
		return
	}
	s.logins.reset(ip)
	if err := s.startSession(w, store.Session{Kind: store.SessionGuest, InviteID: inv.ID, Email: email}, guestSessionTTL); err != nil {
		s.log.Printf("guest login: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.st.Log(store.ActorGuest, email, ip, "guest.login", "code "+inv.Code)
	http.Redirect(w, r, "/form", http.StatusSeeOther)
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	v := loginView{Mode: "admin", Username: username}
	if !s.logins.allowed(ip) {
		s.st.Log(store.ActorAdmin, username, ip, "admin.login_blocked", "too many failed attempts")
		v.Error = "Too many failed attempts from your network. Wait 15 minutes, then try again."
		s.loginFail(w, r, http.StatusTooManyRequests, v)
		return
	}
	a, err := s.st.AdminByUsername(username)
	hash := ""
	if err == nil {
		hash = a.PasswordHash
	}
	if !auth.CheckPassword(hash, password) {
		s.logins.failed(ip)
		s.st.Log(store.ActorAdmin, username, ip, "admin.login_failed", "")
		v.Error = "Wrong username or password."
		s.loginFail(w, r, http.StatusUnauthorized, v)
		return
	}
	s.logins.reset(ip)
	if err := s.startSession(w, store.Session{Kind: store.SessionAdmin, AdminID: a.ID}, adminSessionTTL); err != nil {
		s.log.Printf("admin login: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.st.TouchAdminLogin(a.ID)
	s.st.Log(store.ActorAdmin, a.Username, ip, "admin.login", "")
	http.Redirect(w, r, "/form", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sess, h := s.sessionFrom(r); sess != nil {
		s.st.DeleteSession(h)
		if sess.Kind == store.SessionAdmin {
			if a, err := s.st.AdminByID(sess.AdminID); err == nil {
				s.st.Log(store.ActorAdmin, a.Username, clientIP(r), "admin.logout", "")
			}
		} else {
			s.st.Log(store.ActorGuest, sess.Email, clientIP(r), "guest.logout", "")
		}
	}
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.Secure, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleDone is the confirmation page after a submission. It is reachable
// without a session: a single-use code's session ends when it submits.
func (s *Server) handleDone(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "done.html", page{Title: "Submitted"})
}

// The server is reached directly (no reverse proxy), so RemoteAddr is the client.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limiter counts failed logins per IP in a fixed window.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	m      map[string]*attempts
}

type attempts struct {
	n     int
	first time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, m: map[string]*attempts{}}
}

func (l *limiter) allowed(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.m[ip]
	return a == nil || time.Since(a.first) > l.window || a.n < l.max
}

func (l *limiter) failed(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a := l.m[ip]; a != nil && time.Since(a.first) <= l.window {
		a.n++
		return
	}
	l.m[ip] = &attempts{1, time.Now()}
}

func (l *limiter) reset(ip string) {
	l.mu.Lock()
	delete(l.m, ip)
	l.mu.Unlock()
}

func (l *limiter) purge() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, a := range l.m {
		if time.Since(a.first) > l.window {
			delete(l.m, ip)
		}
	}
}

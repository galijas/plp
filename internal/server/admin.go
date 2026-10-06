package server

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"plportal/internal/auth"
	"plportal/internal/export"
	"plportal/internal/formdef"
	"plportal/internal/mail"
	"plportal/internal/store"
)

func (s *Server) logAdmin(r *http.Request, action, detail string) {
	a := adminFrom(r)
	s.st.Log(store.ActorAdmin, a.Username, clientIP(r), action, detail)
}

func (s *Server) handleFormPage(w http.ResponseWriter, r *http.Request) {
	p := page{Title: "Submission Form", Tab: "form"}
	if g := guestFrom(r); g != nil {
		p.GuestEmail = g.Email
	}
	s.render(w, r, http.StatusOK, "form.html", p)
}

// apiSaveForm stores an edited form as a new version. The client sends the
// version it started from; if another admin saved in between, it gets 409.
func (s *Server) apiSaveForm(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version int64        `json:"version"`
		Form    formdef.Form `json:"form"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "The form could not be read.")
		return
	}
	if err := body.Form.Validate(); err != nil {
		jsonError(w, http.StatusBadRequest, "Not saved: "+err.Error()+".")
		return
	}
	old, _, err := s.st.CurrentForm()
	if err != nil {
		s.internal(w, "load form", err)
		return
	}
	a := adminFrom(r)
	ver, err := s.st.SaveForm(body.Form, body.Version, a.Username)
	if errors.Is(err, store.ErrConflict) {
		jsonError(w, http.StatusConflict, "Another admin saved the form while you were editing. Reload to see their version; your changes here are not saved.")
		return
	}
	if err != nil {
		s.internal(w, "save form", err)
		return
	}
	s.logAdmin(r, "form.edit", fmt.Sprintf("version %d: %s", ver, describeFormChange(old, body.Form)))
	writeJSON(w, http.StatusOK, map[string]any{"version": ver})
}

// describeFormChange summarizes an edit for the action log.
func describeFormChange(old, cur formdef.Form) string {
	type entry struct {
		title string
		item  formdef.Item
	}
	index := func(f formdef.Form) (map[string]entry, []string) {
		m := map[string]entry{}
		var order []string
		for _, s := range f.Sections {
			m[s.ID] = entry{title: "section " + s.Title}
			order = append(order, s.ID)
			for _, it := range s.Items {
				m[it.ID] = entry{title: it.Title, item: it}
				order = append(order, it.ID)
			}
		}
		return m, order
	}
	om, oo := index(old)
	cm, co := index(cur)
	var added, removed, changed []string
	for _, id := range co {
		o, ok := om[id]
		c := cm[id]
		if !ok {
			added = append(added, strconv.Quote(c.title))
			continue
		}
		ob, _ := json.Marshal(o.item)
		cb, _ := json.Marshal(c.item)
		if o.title != c.title || string(ob) != string(cb) {
			changed = append(changed, strconv.Quote(c.title))
		}
	}
	for _, id := range oo {
		if _, ok := cm[id]; !ok {
			removed = append(removed, strconv.Quote(om[id].title))
		}
	}
	var parts []string
	if old.Title != cur.Title || old.Description != cur.Description {
		parts = append(parts, "form title/description")
	}
	if len(added) > 0 {
		parts = append(parts, "added "+strings.Join(added, ", "))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+strings.Join(removed, ", "))
	}
	if len(changed) > 0 {
		parts = append(parts, "changed "+strings.Join(changed, ", "))
	}
	if len(parts) == 0 {
		return "order or section text changed"
	}
	return strings.Join(parts, "; ")
}

// Panel section 1: Submission Explorer (summary; the explorer is its own page).

func (s *Server) handlePanelSubmissions(w http.ResponseWriter, r *http.Request) {
	recent, total, err := s.st.Submissions(store.SubmissionFilter{Limit: 8})
	if err != nil {
		s.log.Printf("submissions: %v", err)
	}
	s.render(w, r, http.StatusOK, "panel_submissions.html", page{Title: "Admin Panel", Tab: "panel", Section: "submissions",
		Data: map[string]any{"Recent": recent, "Total": total}})
}

// Panel section 2: invite codes.

var customCodeRe = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]{5,63}$`)

// expiryChoices are the options offered when creating or changing a code.
var expiryChoices = []struct{ Value, Label string }{
	{"1w", "1 week"}, {"2w", "2 weeks"}, {"1m", "1 month"}, {"3m", "3 months"}, {"never", "No expiry"},
}

func expiryFrom(choice string, from time.Time) (*time.Time, bool) {
	var t time.Time
	switch choice {
	case "1w":
		t = from.AddDate(0, 0, 7)
	case "2w":
		t = from.AddDate(0, 0, 14)
	case "1m":
		t = from.AddDate(0, 1, 0)
	case "3m":
		t = from.AddDate(0, 3, 0)
	case "never":
		return nil, true
	default:
		return nil, false
	}
	return &t, true
}

type inviteRow struct {
	store.Invite
	Status      string
	Submissions int
}

type invitesView struct {
	Rows         []inviteRow
	Expiry       []struct{ Value, Label string }
	PrefillItems []formdef.Item
	NewCode      string // suggested code for the create form
	Highlight    int64
}

func (s *Server) handleInvites(w http.ResponseWriter, r *http.Request) {
	invs, err := s.st.Invites()
	if err != nil {
		s.log.Printf("invites: %v", err)
		s.renderError(w, r, http.StatusInternalServerError, "The invite codes could not be loaded.")
		return
	}
	counts, _ := s.st.InviteSubmissionCounts()
	v := invitesView{Expiry: expiryChoices, NewCode: auth.NewInviteCode()}
	v.Highlight, _ = strconv.ParseInt(r.URL.Query().Get("new"), 10, 64)
	for _, inv := range invs {
		v.Rows = append(v.Rows, inviteRow{Invite: inv, Status: inv.Status(), Submissions: counts[inv.ID]})
	}
	f, _, _ := s.st.CurrentForm()
	for _, sec := range f.Sections {
		for _, it := range sec.Items {
			if it.Type == formdef.Short || it.Type == formdef.Paragraph {
				v.PrefillItems = append(v.PrefillItems, it)
			}
		}
	}
	s.render(w, r, http.StatusOK, "panel_invites.html", page{Title: "Invite Codes", Tab: "panel", Section: "invites", Data: v})
}

// readInviteForm reads the fields shared by create and update.
func (s *Server) readInviteForm(r *http.Request, inv *store.Invite) error {
	inv.Label = strings.TrimSpace(r.PostFormValue("label"))
	if len(inv.Label) > 200 {
		return errors.New("the note can be at most 200 characters")
	}
	if r.PostFormValue("anyEmail") == "1" {
		inv.Email = ""
	} else {
		e, ok := normalizeEmail(r.PostFormValue("email"))
		if !ok {
			return errors.New("enter the client's email address, or choose \"No specific email\"")
		}
		inv.Email = e
	}
	switch r.PostFormValue("uses") {
	case "single":
		inv.MaxUses = 1
	case "unlimited":
		inv.MaxUses = 0
	default:
		return errors.New("choose single use or unlimited")
	}
	f, _, _ := s.st.CurrentForm()
	inv.Prefill = map[string]string{}
	for _, sec := range f.Sections {
		for _, it := range sec.Items {
			if it.Type != formdef.Short && it.Type != formdef.Paragraph {
				continue
			}
			if v := strings.TrimSpace(r.PostFormValue("prefill_" + it.ID)); v != "" {
				if len(v) > 1000 {
					return fmt.Errorf("the pre-filled answer for %q is too long", it.Title)
				}
				inv.Prefill[it.ID] = v
			}
		}
	}
	return nil
}

func (s *Server) handleInviteCreate(w http.ResponseWriter, r *http.Request) {
	a := adminFrom(r)
	inv := &store.Invite{CreatedBy: a.Username}
	if err := s.readInviteForm(r, inv); err != nil {
		s.redirectError(w, r, "/admin/invites", "Not created: "+err.Error()+".")
		return
	}
	code := auth.NormalizeCode(r.PostFormValue("code"))
	if code == "" {
		code = auth.NewInviteCode()
	}
	if !customCodeRe.MatchString(code) {
		s.redirectError(w, r, "/admin/invites", "Not created: a code needs 6 to 64 letters, digits, dashes or underscores.")
		return
	}
	inv.Code = code
	exp, ok := expiryFrom(r.PostFormValue("expiry"), time.Now())
	if !ok {
		s.redirectError(w, r, "/admin/invites", "Not created: choose how long the code stays valid.")
		return
	}
	inv.ExpiresAt = exp
	if err := s.st.CreateInvite(inv); err != nil {
		if errors.Is(err, store.ErrExists) {
			s.redirectError(w, r, "/admin/invites", "Not created: the code "+code+" already exists.")
			return
		}
		s.log.Printf("create invite: %v", err)
		s.redirectError(w, r, "/admin/invites", "The code could not be created.")
		return
	}
	s.logAdmin(r, "invite.create", describeInvite(inv))
	s.redirectFlash(w, r, "/admin/invites?new="+strconv.FormatInt(inv.ID, 10), "Created the code "+code+". Copy its invite link from the highlighted row.")
}

func describeInvite(inv *store.Invite) string {
	who := "any email"
	if inv.Email != "" {
		who = inv.Email
	}
	uses := "unlimited"
	if inv.MaxUses == 1 {
		uses = "single use"
	}
	exp := "no expiry"
	if inv.ExpiresAt != nil {
		exp = "expires " + inv.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC")
	}
	d := fmt.Sprintf("%s for %s, %s, %s", inv.Code, who, uses, exp)
	if inv.Label != "" {
		d += ", note " + strconv.Quote(inv.Label)
	}
	if len(inv.Prefill) > 0 {
		d += fmt.Sprintf(", %d pre-filled answer(s)", len(inv.Prefill))
	}
	return d
}

func (s *Server) inviteFromPath(w http.ResponseWriter, r *http.Request) *store.Invite {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	inv, err := s.st.InviteByID(id)
	if err != nil {
		s.redirectError(w, r, "/admin/invites", "That invite code no longer exists.")
		return nil
	}
	return inv
}

func (s *Server) handleInviteUpdate(w http.ResponseWriter, r *http.Request) {
	inv := s.inviteFromPath(w, r)
	if inv == nil {
		return
	}
	if err := s.readInviteForm(r, inv); err != nil {
		s.redirectError(w, r, "/admin/invites", "Not saved: "+err.Error()+".")
		return
	}
	if choice := r.PostFormValue("expiry"); choice != "keep" {
		exp, ok := expiryFrom(choice, time.Now())
		if !ok {
			s.redirectError(w, r, "/admin/invites", "Not saved: choose how long the code stays valid.")
			return
		}
		inv.ExpiresAt = exp
	}
	if err := s.st.UpdateInvite(inv); err != nil {
		s.log.Printf("update invite: %v", err)
		s.redirectError(w, r, "/admin/invites", "The code could not be saved.")
		return
	}
	// Changing the email of a code ends sessions of other emails on their
	// next request (identify checks AllowsEmail), so nothing else to do.
	s.logAdmin(r, "invite.update", describeInvite(inv))
	s.redirectFlash(w, r, "/admin/invites?new="+strconv.FormatInt(inv.ID, 10), "Saved the code "+inv.Code+".")
}

func (s *Server) handleInviteRevoke(w http.ResponseWriter, r *http.Request) {
	inv := s.inviteFromPath(w, r)
	if inv == nil {
		return
	}
	if err := s.st.SetInviteRevoked(inv.ID, true); err != nil {
		s.redirectError(w, r, "/admin/invites", "The code could not be revoked.")
		return
	}
	s.logAdmin(r, "invite.revoke", inv.Code)
	s.redirectFlash(w, r, "/admin/invites", "Revoked the code "+inv.Code+". Anyone signed in with it was signed out; drafts are kept.")
}

func (s *Server) handleInviteRestore(w http.ResponseWriter, r *http.Request) {
	inv := s.inviteFromPath(w, r)
	if inv == nil {
		return
	}
	if err := s.st.SetInviteRevoked(inv.ID, false); err != nil {
		s.redirectError(w, r, "/admin/invites", "The code could not be restored.")
		return
	}
	s.logAdmin(r, "invite.restore", inv.Code)
	s.redirectFlash(w, r, "/admin/invites", "Restored the code "+inv.Code+".")
}

func (s *Server) handleInviteDelete(w http.ResponseWriter, r *http.Request) {
	inv := s.inviteFromPath(w, r)
	if inv == nil {
		return
	}
	ids, err := s.st.DeleteInvite(inv.ID)
	if err != nil {
		s.redirectError(w, r, "/admin/invites", "The code could not be deleted.")
		return
	}
	for _, id := range ids {
		removeQuiet(s.stagingPath(id))
	}
	s.logAdmin(r, "invite.delete", fmt.Sprintf("%s (with its drafts and %d unsent file(s))", inv.Code, len(ids)))
	s.redirectFlash(w, r, "/admin/invites", "Deleted the code "+inv.Code+" and its unsent drafts. Its submissions are kept.")
}

// Panel section 3: action logs.

type logsView struct {
	Entries                     []store.Entry
	Total, Page, Pages          int
	From, To, Kind, Actor, Text string
	Action                      string
	Actions                     []string
	PrevURL, NextURL, CSVURL    string
}

const logPageSize = 100

func (s *Server) parseLocal(v string) *time.Time {
	if v == "" {
		return nil
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, s.loc); err == nil {
			return &t
		}
	}
	return nil
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v := logsView{From: q.Get("from"), To: q.Get("to"), Kind: q.Get("kind"), Actor: q.Get("actor"), Action: q.Get("action"), Text: q.Get("text")}
	v.Page, _ = strconv.Atoi(q.Get("page"))
	if v.Page < 1 {
		v.Page = 1
	}
	f := store.LogFilter{From: s.parseLocal(v.From), To: s.parseLocal(v.To), ActorKind: v.Kind, Actor: v.Actor, Action: v.Action, Text: v.Text}
	if f.To != nil && len(v.To) == len("2006-01-02") {
		t := f.To.AddDate(0, 0, 1) // a date alone means "through the end of that day"
		f.To = &t
	}
	if q.Get("format") == "csv" {
		f.Limit = 100000
		entries, _, err := s.st.LogEntries(f)
		if err != nil {
			s.renderError(w, r, http.StatusInternalServerError, "The log could not be exported.")
			return
		}
		attachment(w, "Action Log "+time.Now().In(s.loc).Format("2006-01-02 15-04")+".csv", "text/csv; charset=utf-8", false)
		cw := csv.NewWriter(w)
		cw.Write([]string{"Time", "Type", "User", "IP", "Action", "Details"})
		for _, e := range entries {
			cw.Write([]string{e.At.In(s.loc).Format("2006-01-02 15:04:05"), e.ActorKind, csvSafe(e.Actor), e.IP, e.Action, csvSafe(e.Detail)})
		}
		cw.Flush()
		return
	}
	f.Limit, f.Offset = logPageSize, (v.Page-1)*logPageSize
	entries, total, err := s.st.LogEntries(f)
	if err != nil {
		s.log.Printf("logs: %v", err)
		s.renderError(w, r, http.StatusInternalServerError, "The log could not be loaded.")
		return
	}
	v.Entries, v.Total = entries, total
	v.Pages = max(1, (total+logPageSize-1)/logPageSize)
	v.Actions, _ = s.st.LogActions()
	link := func(p int) string {
		qq := r.URL.Query()
		qq.Set("page", strconv.Itoa(p))
		qq.Del("format")
		return "/admin/logs?" + qq.Encode()
	}
	if v.Page > 1 {
		v.PrevURL = link(v.Page - 1)
	}
	if v.Page < v.Pages {
		v.NextURL = link(v.Page + 1)
	}
	cq := r.URL.Query()
	cq.Del("page")
	cq.Set("format", "csv")
	v.CSVURL = "/admin/logs?" + cq.Encode()
	s.render(w, r, http.StatusOK, "panel_logs.html", page{Title: "Action Logs", Tab: "panel", Section: "logs", Data: v})
}

// csvSafe keeps spreadsheet apps from treating guest-typed text as a formula.
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

// Panel section 4: accounts.

type accountsView struct {
	Admins      []store.Admin
	NewUser     string
	NewPassword string
}

func (s *Server) renderAccounts(w http.ResponseWriter, r *http.Request, v accountsView) {
	admins, err := s.st.Admins()
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "The accounts could not be loaded.")
		return
	}
	v.Admins = admins
	s.render(w, r, http.StatusOK, "panel_accounts.html", page{Title: "Accounts", Tab: "panel", Section: "accounts", Data: v})
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	s.renderAccounts(w, r, accountsView{})
}

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{2,40}$`)

func (s *Server) handleAccountCreate(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.PostFormValue("username"))
	if !usernameRe.MatchString(username) {
		s.redirectError(w, r, "/admin/accounts", "Not created: a username needs 2 to 40 letters, digits, dots, dashes or underscores.")
		return
	}
	email := ""
	if e := strings.TrimSpace(r.PostFormValue("email")); e != "" {
		var ok bool
		if email, ok = normalizeEmail(e); !ok {
			s.redirectError(w, r, "/admin/accounts", "Not created: the email address is not valid.")
			return
		}
	}
	pw := auth.GeneratePassword()
	hash, _ := auth.HashPassword(pw)
	if _, err := s.st.CreateAdmin(username, email, hash, adminFrom(r).Username); err != nil {
		if errors.Is(err, store.ErrExists) {
			s.redirectError(w, r, "/admin/accounts", "Not created: the username "+username+" is taken.")
			return
		}
		s.log.Printf("create admin: %v", err)
		s.redirectError(w, r, "/admin/accounts", "The account could not be created.")
		return
	}
	s.logAdmin(r, "account.create", username)
	s.renderAccounts(w, r, accountsView{NewUser: username, NewPassword: pw})
}

func (s *Server) accountFromPath(w http.ResponseWriter, r *http.Request) *store.Admin {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a, err := s.st.AdminByID(id)
	if err != nil {
		s.redirectError(w, r, "/admin/accounts", "That account no longer exists.")
		return nil
	}
	return a
}

func (s *Server) handleAccountReset(w http.ResponseWriter, r *http.Request) {
	a := s.accountFromPath(w, r)
	if a == nil {
		return
	}
	if a.ID == adminFrom(r).ID {
		s.redirectError(w, r, "/admin/security", "Change your own password here, in Security.")
		return
	}
	pw := auth.GeneratePassword()
	hash, _ := auth.HashPassword(pw)
	if err := s.st.SetAdminPassword(a.ID, hash, ""); err != nil {
		s.redirectError(w, r, "/admin/accounts", "The password could not be reset.")
		return
	}
	s.logAdmin(r, "account.reset_password", a.Username)
	s.renderAccounts(w, r, accountsView{NewUser: a.Username, NewPassword: pw})
}

func (s *Server) handleAccountEmail(w http.ResponseWriter, r *http.Request) {
	a := s.accountFromPath(w, r)
	if a == nil {
		return
	}
	email := ""
	if e := strings.TrimSpace(r.PostFormValue("email")); e != "" {
		var ok bool
		if email, ok = normalizeEmail(e); !ok {
			s.redirectError(w, r, "/admin/accounts", "Not saved: the email address is not valid.")
			return
		}
	}
	s.st.SetAdminEmail(a.ID, email)
	s.logAdmin(r, "account.email", fmt.Sprintf("%s: %q", a.Username, email))
	s.redirectFlash(w, r, "/admin/accounts", "Saved the email of "+a.Username+".")
}

func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	a := s.accountFromPath(w, r)
	if a == nil {
		return
	}
	if a.ID == adminFrom(r).ID {
		s.redirectError(w, r, "/admin/accounts", "You can't delete your own account. Ask another admin.")
		return
	}
	if err := s.st.DeleteAdmin(a.ID); err != nil {
		s.redirectError(w, r, "/admin/accounts", "Not deleted: "+err.Error()+".")
		return
	}
	s.logAdmin(r, "account.delete", a.Username)
	s.redirectFlash(w, r, "/admin/accounts", "Deleted the account "+a.Username+".")
}

// Panel section 5: security (own password and notification email).

func (s *Server) handleSecurity(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "panel_security.html", page{Title: "Security", Tab: "panel", Section: "security"})
}

func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	a := adminFrom(r)
	cur, pw, again := r.PostFormValue("current"), r.PostFormValue("new"), r.PostFormValue("confirm")
	if !auth.CheckPassword(a.PasswordHash, cur) {
		s.logAdmin(r, "account.password_change_failed", "wrong current password")
		s.redirectError(w, r, "/admin/security", "Not changed: the current password is wrong.")
		return
	}
	if pw != again {
		s.redirectError(w, r, "/admin/security", "Not changed: the new passwords don't match.")
		return
	}
	if pw == cur {
		s.redirectError(w, r, "/admin/security", "Not changed: the new password is the same as the current one.")
		return
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		s.redirectError(w, r, "/admin/security", "Not changed: "+err.Error()+".")
		return
	}
	_, keep := s.sessionFrom(r)
	if err := s.st.SetAdminPassword(a.ID, hash, keep); err != nil {
		s.redirectError(w, r, "/admin/security", "The password could not be changed.")
		return
	}
	s.logAdmin(r, "account.password_change", "")
	s.redirectFlash(w, r, "/admin/security", "Password changed. Your other sessions were signed out.")
}

func (s *Server) handleOwnEmail(w http.ResponseWriter, r *http.Request) {
	a := adminFrom(r)
	email := ""
	if e := strings.TrimSpace(r.PostFormValue("email")); e != "" {
		var ok bool
		if email, ok = normalizeEmail(e); !ok {
			s.redirectError(w, r, "/admin/security", "Not saved: the email address is not valid.")
			return
		}
	}
	s.st.SetAdminEmail(a.ID, email)
	s.logAdmin(r, "account.email", fmt.Sprintf("%s: %q", a.Username, email))
	s.redirectFlash(w, r, "/admin/security", "Saved your notification email.")
}

// Panel section 6: SMTP for submission notifications.

const smtpKey = "smtp"

func (s *Server) smtpConfig() mail.Config {
	c := mail.Config{Port: 587, Security: mail.StartTLS, FromName: "Private Label Portal"}
	if v, _ := s.st.Setting(smtpKey); v != "" {
		json.Unmarshal([]byte(v), &c)
	}
	return c
}

type smtpView struct {
	Config      mail.Config
	HasPassword bool
	Admins      []store.Admin
}

func (s *Server) handleSMTP(w http.ResponseWriter, r *http.Request) {
	c := s.smtpConfig()
	admins, _ := s.st.Admins()
	v := smtpView{Config: c, HasPassword: c.Password != "", Admins: admins}
	v.Config.Password = ""
	s.render(w, r, http.StatusOK, "panel_smtp.html", page{Title: "Configure SMTP", Tab: "panel", Section: "smtp", Data: v})
}

func (s *Server) handleSMTPSave(w http.ResponseWriter, r *http.Request) {
	old := s.smtpConfig()
	port, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("port")))
	c := mail.Config{
		Enabled:  r.PostFormValue("enabled") == "1",
		Host:     r.PostFormValue("host"),
		Port:     port,
		Security: r.PostFormValue("security"),
		Username: r.PostFormValue("username"),
		Password: r.PostFormValue("password"),
		From:     r.PostFormValue("from"),
		FromName: strings.TrimSpace(r.PostFormValue("fromName")),
	}
	if c.Password == "" && r.PostFormValue("clearPassword") != "1" {
		c.Password = old.Password // blank field keeps the saved password
	}
	if c.Enabled || c.Host != "" {
		if err := c.Validate(); err != nil {
			s.redirectError(w, r, "/admin/smtp", "Not saved: "+err.Error()+".")
			return
		}
	}
	b, _ := json.Marshal(c)
	if err := s.st.SetSetting(smtpKey, string(b)); err != nil {
		s.redirectError(w, r, "/admin/smtp", "The SMTP settings could not be saved.")
		return
	}
	state := "disabled"
	if c.Enabled {
		state = "enabled"
	}
	s.logAdmin(r, "smtp.update", fmt.Sprintf("%s, %s:%d (%s), from %s", state, c.Host, c.Port, c.Security, c.From))
	s.redirectFlash(w, r, "/admin/smtp", "Saved the SMTP settings.")
}

func (s *Server) handleSMTPTest(w http.ResponseWriter, r *http.Request) {
	a := adminFrom(r)
	c := s.smtpConfig()
	if c.Host == "" {
		s.redirectError(w, r, "/admin/smtp", "Save the SMTP settings first.")
		return
	}
	if a.Email == "" {
		s.redirectError(w, r, "/admin/smtp", "Your account has no email address. Add one in Security, then send the test.")
		return
	}
	body := fmt.Sprintf("This is a test email from the Private Label Portal at %s.\n\nSent by %s. Submission notifications will arrive like this one.\n", s.publicURL(), a.Username)
	err := mail.Send(c, []string{a.Email}, "Private Label Portal: test email", body)
	if err != nil {
		s.logAdmin(r, "smtp.test_failed", err.Error())
		s.redirectError(w, r, "/admin/smtp", "The test email failed: "+err.Error())
		return
	}
	s.logAdmin(r, "smtp.test", "sent to "+a.Email)
	s.redirectFlash(w, r, "/admin/smtp", "Sent a test email to "+a.Email+".")
}

// notifySubmission emails every admin that has an address. The email has no
// answers in it (they can include secrets such as API keys); it links to
// the submission instead.
func (s *Server) notifySubmission(sub *store.Submission) {
	c := s.smtpConfig()
	if !c.Enabled {
		return
	}
	to, err := s.st.AdminEmails()
	if err != nil || len(to) == 0 {
		return
	}
	label := sub.Label
	if label == "" {
		label = sub.Email
	}
	body := fmt.Sprintf("A new form was submitted.\n\nCompany:      %s\nSubmitted by: %s\nTime:         %s\nFiles:        %d (%s)\nInvite code:  %s\n\nOpen it in the Submission Explorer:\n%s/explorer/%s\n",
		sub.Label, sub.Email, sub.SubmittedAt.In(s.loc).Format("2006-01-02 15:04 MST"),
		sub.FilesCount, export.HumanBytes(sub.FilesBytes), sub.InviteCode, s.publicURL(), sub.ID)
	if err := mail.Send(c, to, "New submission: "+label, body); err != nil {
		s.log.Printf("notify: %v", err)
		s.st.Log(store.ActorSystem, "notifier", "", "smtp.notify_failed", sub.Folder+": "+err.Error())
		return
	}
	s.st.Log(store.ActorSystem, "notifier", "", "smtp.notify", fmt.Sprintf("%s: sent to %d admin(s)", sub.Folder, len(to)))
}

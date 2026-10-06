package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"plportal/internal/auth"
	"plportal/internal/formdef"
	"plportal/internal/store"
)

const (
	maxDraftBody = 1 << 20
	maxChunk     = 16 << 20
	// diskReserve is kept free on the data disk; uploads that would eat into
	// it are refused.
	diskReserve = 2 << 30
)

type uploadView struct {
	ID       string `json:"id"`
	ItemID   string `json:"itemId"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Received int64  `json:"received"`
	Complete bool   `json:"complete"`
}

func toUploadView(u store.Upload) uploadView {
	return uploadView{ID: u.ID, ItemID: u.ItemID, Name: u.Name, Size: u.Size, Received: u.Received, Complete: u.Complete()}
}

// apiForm returns the form; for guests also their draft and uploads.
func (s *Server) apiForm(w http.ResponseWriter, r *http.Request) {
	f, ver, err := s.st.CurrentForm()
	if err != nil {
		s.internal(w, "load form", err)
		return
	}
	resp := map[string]any{
		"form":          f,
		"version":       ver,
		"maxTotalBytes": s.cfg.MaxSubmissionBytes,
		"maxChunkBytes": maxChunk,
	}
	if a := adminFrom(r); a != nil {
		resp["role"] = "admin"
		resp["user"] = a.Username
		writeJSON(w, http.StatusOK, resp)
		return
	}
	g := guestFrom(r)
	answers, savedAt, err := s.st.Draft(g.Invite.ID, g.Email)
	if err != nil {
		s.internal(w, "load draft", err)
		return
	}
	if answers == nil {
		// A new draft starts from the answers the admin pre-filled on the invite.
		answers = formdef.Answers{}
		for id, text := range g.Invite.Prefill {
			answers[id] = formdef.Answer{Text: text}
		}
		answers = f.Clean(answers)
	}
	ups, err := s.st.Uploads(g.Invite.ID, g.Email)
	if err != nil {
		s.internal(w, "load uploads", err)
		return
	}
	views := []uploadView{}
	for _, u := range ups {
		views = append(views, toUploadView(u))
	}
	resp["role"] = "guest"
	resp["user"] = g.Email
	resp["answers"] = answers
	resp["uploads"] = views
	if savedAt != nil {
		resp["savedAt"] = savedAt.In(s.loc).Format("15:04")
	}
	resp["singleUse"] = g.Invite.MaxUses == 1
	if g.Invite.ExpiresAt != nil {
		resp["expiresAt"] = g.Invite.ExpiresAt.In(s.loc).Format("2006-01-02 15:04 MST")
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) internal(w http.ResponseWriter, what string, err error) {
	s.log.Printf("%s: %v", what, err)
	jsonError(w, http.StatusInternalServerError, "Something went wrong on the server. Try again in a minute.")
}

func (s *Server) apiSaveDraft(w http.ResponseWriter, r *http.Request) {
	g := guestFrom(r)
	var body struct {
		Answers formdef.Answers `json:"answers"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDraftBody)).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "The draft could not be read.")
		return
	}
	f, _, err := s.st.CurrentForm()
	if err != nil {
		s.internal(w, "load form", err)
		return
	}
	_, prev, _ := s.st.Draft(g.Invite.ID, g.Email)
	t, err := s.st.SaveDraft(g.Invite.ID, g.Email, f.Clean(body.Answers))
	if err != nil {
		s.internal(w, "save draft", err)
		return
	}
	if prev == nil {
		s.st.Log(store.ActorGuest, g.Email, clientIP(r), "guest.draft_started", "code "+g.Invite.Code)
	}
	writeJSON(w, http.StatusOK, map[string]string{"savedAt": t.In(s.loc).Format("15:04")})
}

func (s *Server) stagingPath(id string) string { return filepath.Join(s.stagingDir(), id) }

// cleanFileName keeps the base name of an uploaded file, without path
// separators, control characters or names that mean something to a shell
// or file system.
func cleanFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = name[strings.LastIndex(name, "/")+1:]
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(strings.Trim(name, ". "))
	for len(name) > 180 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	if name == "" {
		name = "file"
	}
	return name
}

func freeBytes(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

func (s *Server) apiUploadCreate(w http.ResponseWriter, r *http.Request) {
	g := guestFrom(r)
	var body struct {
		ItemID string `json:"itemId"`
		Name   string `json:"name"`
		Size   int64  `json:"size"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "The upload request could not be read.")
		return
	}
	f, _, err := s.st.CurrentForm()
	if err != nil {
		s.internal(w, "load form", err)
		return
	}
	it, ok := f.Item(body.ItemID)
	if !ok || it.Type != formdef.File {
		jsonError(w, http.StatusBadRequest, "This question doesn't take files. Reload the page; the form may have changed.")
		return
	}
	name := cleanFileName(body.Name)
	ext := strings.ToLower(filepath.Ext(name))
	if len(it.Accept) > 0 && !slices.Contains(it.Accept, ext) {
		jsonError(w, http.StatusBadRequest, fmt.Sprintf("%s: this question accepts %s files.", name, strings.Join(it.Accept, ", ")))
		return
	}
	if body.Size <= 0 {
		jsonError(w, http.StatusBadRequest, name+" is empty.")
		return
	}
	if body.Size > int64(it.MaxFileMB)<<20 {
		jsonError(w, http.StatusBadRequest, fmt.Sprintf("%s is larger than the %d MB limit for this question.", name, it.MaxFileMB))
		return
	}
	ups, err := s.st.Uploads(g.Invite.ID, g.Email)
	if err != nil {
		s.internal(w, "list uploads", err)
		return
	}
	var total int64
	count := 0
	for _, u := range ups {
		total += u.Size
		if u.ItemID == it.ID {
			count++
		}
	}
	if count >= it.MaxFiles {
		jsonError(w, http.StatusBadRequest, fmt.Sprintf("This question takes at most %d file(s). Remove one to add another.", it.MaxFiles))
		return
	}
	if total+body.Size > s.cfg.MaxSubmissionBytes {
		jsonError(w, http.StatusBadRequest, fmt.Sprintf("All files together can be at most %d MB.", s.cfg.MaxSubmissionBytes>>20))
		return
	}
	if free := freeBytes(s.stagingDir()); free >= 0 && free-body.Size < diskReserve {
		s.log.Printf("upload refused: disk almost full (%d bytes free)", free)
		s.st.Log(store.ActorSystem, "server", "", "system.disk_full", fmt.Sprintf("refused %s (%d bytes) from %s", name, body.Size, g.Email))
		jsonError(w, http.StatusInsufficientStorage, "The server can't take more files right now. Send the files by email on the project ticket thread, or try again later.")
		return
	}
	u := store.Upload{ID: auth.NewID(), InviteID: g.Invite.ID, Email: g.Email, ItemID: it.ID, Name: name, Size: body.Size}
	fh, err := os.OpenFile(s.stagingPath(u.ID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		s.internal(w, "create staging file", err)
		return
	}
	fh.Close()
	if err := s.st.CreateUpload(&u); err != nil {
		os.Remove(s.stagingPath(u.ID))
		s.internal(w, "create upload", err)
		return
	}
	writeJSON(w, http.StatusCreated, toUploadView(u))
}

func (s *Server) lockFor(m *sync.Map, key string) *sync.Mutex {
	l, _ := m.LoadOrStore(key, &sync.Mutex{})
	return l.(*sync.Mutex)
}

// ownUpload loads an upload that belongs to the guest's draft.
func (s *Server) ownUpload(w http.ResponseWriter, r *http.Request) *store.Upload {
	g := guestFrom(r)
	u, err := s.st.UploadByID(r.PathValue("id"))
	if err != nil || u.InviteID != g.Invite.ID || u.Email != g.Email {
		jsonError(w, http.StatusNotFound, "This upload no longer exists. Add the file again.")
		return nil
	}
	return u
}

// apiUploadChunk appends one chunk. The client sends Upload-Offset with the
// byte position it is sending from; it must match what the server has, so
// a retried chunk is never written twice. A mismatch returns the server's
// position (409) and the client resumes from there.
func (s *Server) apiUploadChunk(w http.ResponseWriter, r *http.Request) {
	l := s.lockFor(&s.uploadLocks, r.PathValue("id"))
	l.Lock()
	defer l.Unlock()
	u := s.ownUpload(w, r)
	if u == nil {
		return
	}
	if u.Complete() {
		writeJSON(w, http.StatusOK, toUploadView(*u))
		return
	}
	off, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || off != u.Received {
		writeJSON(w, http.StatusConflict, toUploadView(*u))
		return
	}
	limit := min(u.Size-u.Received, maxChunk)
	fh, err := os.OpenFile(s.stagingPath(u.ID), os.O_WRONLY, 0o600)
	if err != nil {
		s.internal(w, "open staging file", err)
		return
	}
	defer fh.Close()
	if err := fh.Truncate(u.Received); err != nil {
		s.internal(w, "truncate staging file", err)
		return
	}
	if _, err := fh.Seek(u.Received, io.SeekStart); err != nil {
		s.internal(w, "seek staging file", err)
		return
	}
	// Read one byte past the limit to notice a client sending too much.
	n, err := io.Copy(fh, io.LimitReader(r.Body, limit+1))
	if n > limit {
		fh.Truncate(u.Received)
		jsonError(w, http.StatusRequestEntityTooLarge, "The file is larger than announced. Add it again.")
		return
	}
	if err != nil {
		// Drop the partial chunk; the client resends it from the stored position.
		fh.Truncate(u.Received)
		jsonError(w, http.StatusBadRequest, "The connection broke during the upload. It will resume.")
		return
	}
	if err := fh.Sync(); err != nil {
		s.internal(w, "sync staging file", err)
		return
	}
	u.Received += n
	complete := u.Received == u.Size
	if err := s.st.SetUploadReceived(u.ID, u.Received, complete); err != nil {
		s.internal(w, "update upload", err)
		return
	}
	if complete {
		now := time.Now()
		u.CompletedAt = &now
		g := guestFrom(r)
		s.st.Log(store.ActorGuest, g.Email, clientIP(r), "guest.upload", fmt.Sprintf("%s (%d bytes) for %s", u.Name, u.Size, u.ItemID))
	}
	writeJSON(w, http.StatusOK, toUploadView(*u))
}

func (s *Server) apiUploadDelete(w http.ResponseWriter, r *http.Request) {
	l := s.lockFor(&s.uploadLocks, r.PathValue("id"))
	l.Lock()
	defer l.Unlock()
	u := s.ownUpload(w, r)
	if u == nil {
		return
	}
	if err := s.st.DeleteUpload(u.ID); err != nil {
		s.internal(w, "delete upload", err)
		return
	}
	os.Remove(s.stagingPath(u.ID))
	if u.Complete() {
		s.st.Log(store.ActorGuest, u.Email, clientIP(r), "guest.upload_removed", u.Name+" from "+u.ItemID)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) apiSubmit(w http.ResponseWriter, r *http.Request) {
	g := guestFrom(r)
	// One submission at a time per guest: a double click must not submit twice.
	l := s.lockFor(&s.submitLocks, strconv.FormatInt(g.Invite.ID, 10)+"/"+g.Email)
	if !l.TryLock() {
		jsonError(w, http.StatusConflict, "Your submission is already being processed.")
		return
	}
	defer l.Unlock()

	var body struct {
		Answers formdef.Answers `json:"answers"`
		Version int64           `json:"version"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDraftBody)).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "The answers could not be read.")
		return
	}
	f, ver, err := s.st.CurrentForm()
	if err != nil {
		s.internal(w, "load form", err)
		return
	}
	answers := f.Clean(body.Answers)
	// Save first, so nothing typed is lost if the submission is refused.
	s.st.SaveDraft(g.Invite.ID, g.Email, answers)
	if body.Version != ver {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":       "The form was updated while you were filling it in. Your answers are saved; review the updated form and submit again.",
			"formChanged": true,
		})
		return
	}
	ups, err := s.st.Uploads(g.Invite.ID, g.Email)
	if err != nil {
		s.internal(w, "list uploads", err)
		return
	}
	counts := map[string]int{}
	var done []store.Upload
	for _, u := range ups {
		if !u.Complete() {
			jsonError(w, http.StatusBadRequest, "Wait until all files have finished uploading, then submit.")
			return
		}
		if it, ok := f.Item(u.ItemID); ok && it.Type == formdef.File {
			counts[u.ItemID]++
			done = append(done, u)
		}
	}
	if miss := f.Missing(answers, counts); len(miss) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "Some required questions have no answer yet.",
			"missing": miss,
		})
		return
	}
	sub, err := s.createSubmission(g, f, ver, answers, done)
	if err != nil {
		if errors.Is(err, store.ErrInviteUnusable) {
			jsonError(w, http.StatusForbidden, "Your invite code is no longer valid, so the form can't be submitted. Your answers are saved; ask your Bicom Systems contact to extend the code.")
			return
		}
		s.internal(w, "submit", err)
		return
	}
	for _, u := range done {
		os.Remove(s.stagingPath(u.ID))
	}
	s.st.Log(store.ActorGuest, g.Email, clientIP(r), "guest.submit",
		fmt.Sprintf("%s: %d file(s), code %s", sub.Folder, sub.FilesCount, g.Invite.Code))
	go s.notifySubmission(sub)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "redirect": "/done"})
}

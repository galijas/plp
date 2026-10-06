package server

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"plportal/internal/auth"
	"plportal/internal/export"
	"plportal/internal/formdef"
	"plportal/internal/store"
)

// Each submission gets a folder under data/submissions named after the
// email and time, holding:
//
//	Form Answers - <email> - <time>.txt
//	Uploaded Files - <email> - <time>.zip   (only when files were uploaded)
//
// The PDF of the answers is generated from the stored snapshot on request.

func folderName(email string, t time.Time) string {
	n := cleanFileName(email + " - " + export.Stamp(t))
	return strings.ReplaceAll(n, "/", "_")
}

func (s *Server) folderPath(sub *store.Submission) string {
	return filepath.Join(s.submissionsDir(), sub.Folder)
}

func (s *Server) createSubmission(g *guestCtxValue, f formdef.Form, ver int64, answers formdef.Answers, uploads []store.Upload) (*store.Submission, error) {
	at := time.Now().In(s.loc).Truncate(time.Second)
	folder := folderName(g.Email, at)
	for i := 2; s.st.FolderExists(folder) || exists(filepath.Join(s.submissionsDir(), folder)); i++ {
		folder = folderName(g.Email, at) + fmt.Sprintf(" (%d)", i)
	}

	// Files go into the zip under a folder per question, named after the
	// question title, with duplicate names numbered.
	var refs []formdef.FileRef
	var total int64
	type entry struct {
		path string
		up   store.Upload
	}
	var entries []entry
	used := map[string]bool{}
	order := map[string]int{}
	for _, sec := range f.Sections {
		for _, it := range sec.Items {
			order[it.ID] = len(order)
		}
	}
	slices.SortStableFunc(uploads, func(a, b store.Upload) int { return order[a.ItemID] - order[b.ItemID] })
	for _, u := range uploads {
		it, _ := f.Item(u.ItemID)
		dir := cleanFileName(strings.TrimSuffix(it.Title, ":"))
		name := u.Name
		p := dir + "/" + name
		for i := 2; used[strings.ToLower(p)]; i++ {
			ext := filepath.Ext(name)
			p = fmt.Sprintf("%s/%s (%d)%s", dir, strings.TrimSuffix(name, ext), i, ext)
		}
		used[strings.ToLower(p)] = true
		entries = append(entries, entry{p, u})
		refs = append(refs, formdef.FileRef{ItemID: u.ItemID, Name: p[len(dir)+1:]})
		total += u.Size
	}

	snap := f.Snapshot(answers, refs)
	sub := &store.Submission{
		ID: auth.NewID(), InviteCode: g.Invite.Code, Email: g.Email, Label: snap.FirstText(),
		SubmittedAt: at, FormVersion: ver, Snapshot: snap, Folder: folder,
		FilesCount: len(entries), FilesBytes: total,
	}

	dir := s.folderPath(sub)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	fail := func(err error) (*store.Submission, error) {
		os.RemoveAll(dir)
		return nil, err
	}
	var txt bytes.Buffer
	export.WriteText(&txt, snap, s.meta(sub))
	if err := os.WriteFile(filepath.Join(dir, export.AnswersName(sub.Email, at, ".txt")), txt.Bytes(), 0o600); err != nil {
		return fail(err)
	}
	if len(entries) > 0 {
		zf, err := os.OpenFile(filepath.Join(dir, export.FilesName(sub.Email, at)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fail(err)
		}
		zw := zip.NewWriter(zf)
		for _, e := range entries {
			// Stored, not compressed: branding files are mostly zips and
			// images already, and storing keeps a 1 GB submission quick.
			w, err := zw.CreateHeader(&zip.FileHeader{Name: e.path, Method: zip.Store, Modified: e.up.CreatedAt})
			if err != nil {
				zf.Close()
				return fail(err)
			}
			src, err := os.Open(s.stagingPath(e.up.ID))
			if err != nil {
				zf.Close()
				return fail(err)
			}
			_, err = io.Copy(w, src)
			src.Close()
			if err != nil {
				zf.Close()
				return fail(err)
			}
		}
		if err := zw.Close(); err != nil {
			zf.Close()
			return fail(err)
		}
		if err := zf.Sync(); err != nil {
			zf.Close()
			return fail(err)
		}
		if err := zf.Close(); err != nil {
			return fail(err)
		}
	}
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.up.ID)
	}
	if err := s.st.FinishSubmission(sub, g.Invite.ID, ids); err != nil {
		return fail(err)
	}
	return sub, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (s *Server) meta(sub *store.Submission) export.Meta {
	return export.Meta{Email: sub.Email, SubmittedAt: sub.SubmittedAt.In(s.loc), InviteCode: sub.InviteCode,
		FilesCount: sub.FilesCount, FilesBytes: sub.FilesBytes}
}

// Submission explorer.

type explorerView struct {
	Items    []store.Submission
	Total    int
	Query    string
	From, To string
	Page     int
	Pages    int
	PrevURL  string
	NextURL  string
}

const explorerPageSize = 40

func (s *Server) parseDay(v string, endOfDay bool) *time.Time {
	if v == "" {
		return nil
	}
	t, err := time.ParseInLocation("2006-01-02", v, s.loc)
	if err != nil {
		return nil
	}
	if endOfDay {
		t = t.AddDate(0, 0, 1)
	}
	return &t
}

func (s *Server) handleExplorer(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v := explorerView{Query: q.Get("q"), From: q.Get("from"), To: q.Get("to")}
	v.Page, _ = strconv.Atoi(q.Get("page"))
	if v.Page < 1 {
		v.Page = 1
	}
	items, total, err := s.st.Submissions(store.SubmissionFilter{
		Query: v.Query, From: s.parseDay(v.From, false), To: s.parseDay(v.To, true),
		Limit: explorerPageSize, Offset: (v.Page - 1) * explorerPageSize,
	})
	if err != nil {
		s.log.Printf("explorer: %v", err)
		s.renderError(w, r, http.StatusInternalServerError, "The submissions could not be loaded.")
		return
	}
	v.Items, v.Total = items, total
	v.Pages = max(1, (total+explorerPageSize-1)/explorerPageSize)
	link := func(p int) string {
		qq := r.URL.Query()
		qq.Set("page", strconv.Itoa(p))
		return "/explorer?" + qq.Encode()
	}
	if v.Page > 1 {
		v.PrevURL = link(v.Page - 1)
	}
	if v.Page < v.Pages {
		v.NextURL = link(v.Page + 1)
	}
	s.render(w, r, http.StatusOK, "explorer.html", page{Title: "Submission Explorer", Tab: "explorer", Data: v})
}

type folderView struct {
	Sub        *store.Submission
	AnswersTxt string
	AnswersPDF string
	FilesZip   string
	ZipEntries []zipEntry
	ZipMissing bool
}

type zipEntry struct {
	Name string
	Size int64
}

func (s *Server) loadSubmission(w http.ResponseWriter, r *http.Request) *store.Submission {
	sub, err := s.st.SubmissionByID(r.PathValue("id"))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "This submission doesn't exist. It may have been deleted.")
		return nil
	}
	return sub
}

func (s *Server) handleFolder(w http.ResponseWriter, r *http.Request) {
	sub := s.loadSubmission(w, r)
	if sub == nil {
		return
	}
	at := sub.SubmittedAt.In(s.loc)
	v := folderView{
		Sub:        sub,
		AnswersTxt: export.AnswersName(sub.Email, at, ".txt"),
		AnswersPDF: export.AnswersName(sub.Email, at, ".pdf"),
		FilesZip:   export.FilesName(sub.Email, at),
	}
	if sub.FilesCount > 0 {
		zr, err := zip.OpenReader(filepath.Join(s.folderPath(sub), v.FilesZip))
		if err != nil {
			v.ZipMissing = true
		} else {
			for _, f := range zr.File {
				v.ZipEntries = append(v.ZipEntries, zipEntry{f.Name, int64(f.UncompressedSize64)})
			}
			zr.Close()
		}
	}
	s.render(w, r, http.StatusOK, "folder.html", page{Title: sub.Folder, Tab: "explorer", Data: v})
}

func attachment(w http.ResponseWriter, name, ctype string, inline bool) {
	disp := "attachment"
	if inline {
		disp = "inline"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "no-store")
}

func (s *Server) handleAnswersText(w http.ResponseWriter, r *http.Request) {
	sub := s.loadSubmission(w, r)
	if sub == nil {
		return
	}
	name := export.AnswersName(sub.Email, sub.SubmittedAt.In(s.loc), ".txt")
	attachment(w, name, "text/plain; charset=utf-8", false)
	// The stored file is the original; regenerate only if it went missing.
	if b, err := os.ReadFile(filepath.Join(s.folderPath(sub), name)); err == nil {
		w.Write(b)
		return
	}
	export.WriteText(w, sub.Snapshot, s.meta(sub))
}

func (s *Server) handleAnswersPDF(w http.ResponseWriter, r *http.Request) {
	sub := s.loadSubmission(w, r)
	if sub == nil {
		return
	}
	var buf bytes.Buffer
	if err := export.WritePDF(&buf, sub.Snapshot, s.meta(sub)); err != nil {
		s.log.Printf("pdf %s: %v", sub.ID, err)
		s.renderError(w, r, http.StatusInternalServerError, "The PDF could not be created.")
		return
	}
	name := export.AnswersName(sub.Email, sub.SubmittedAt.In(s.loc), ".pdf")
	attachment(w, name, "application/pdf", r.URL.Query().Get("view") == "1")
	w.Write(buf.Bytes())
}

func (s *Server) handleFilesZip(w http.ResponseWriter, r *http.Request) {
	sub := s.loadSubmission(w, r)
	if sub == nil {
		return
	}
	name := export.FilesName(sub.Email, sub.SubmittedAt.In(s.loc))
	f, err := os.Open(filepath.Join(s.folderPath(sub), name))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "This submission has no uploaded files.")
		return
	}
	defer f.Close()
	a := adminFrom(r)
	s.st.Log(store.ActorAdmin, a.Username, clientIP(r), "submission.download", sub.Folder)
	attachment(w, name, "application/zip", false)
	st, _ := f.Stat()
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func (s *Server) handleSubmissionDelete(w http.ResponseWriter, r *http.Request) {
	sub := s.loadSubmission(w, r)
	if sub == nil {
		return
	}
	if r.PostFormValue("confirm") != sub.Email {
		s.redirectError(w, r, "/explorer/"+sub.ID, "Type the submitter's email to confirm the deletion.")
		return
	}
	if err := s.st.DeleteSubmission(sub.ID); err != nil {
		s.log.Printf("delete submission: %v", err)
		s.redirectError(w, r, "/explorer/"+sub.ID, "The submission could not be deleted.")
		return
	}
	os.RemoveAll(s.folderPath(sub))
	a := adminFrom(r)
	s.st.Log(store.ActorAdmin, a.Username, clientIP(r), "submission.delete", sub.Folder)
	s.redirectFlash(w, r, "/explorer", "Deleted the submission "+sub.Folder+".")
}

func removeQuiet(p string) { os.Remove(p) }

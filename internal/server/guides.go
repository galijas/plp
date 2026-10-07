package server

import (
	"archive/zip"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Guides are PDFs that form descriptions link to, kept in data/guides on
// the server (not in the repository, which is public) and served only to
// signed-in clients and admins. Form links use guideURL(file name).
// To replace a guide, copy a PDF with the same name into that directory.

const allGuidesName = "Private Label Guides.zip"

func (s *Server) guidesDir() string { return filepath.Join(s.cfg.DataDir, "guides") }

// guideURL is the link to put in a form description.
func guideURL(name string) string { return "/guides/" + url.PathEscape(name) }

// guides lists the PDF file names in the guides directory, sorted.
func (s *Server) guides() []string {
	entries, err := os.ReadDir(s.guidesDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.EqualFold(filepath.Ext(e.Name()), ".pdf") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// handleGuide shows one guide in the browser (inline, so it opens in the
// new tab; the browser's PDF viewer offers the download). Only names that
// are listed in the directory are served, so no path tricks get through.
func (s *Server) handleGuide(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for _, g := range s.guides() {
		if g == name {
			attachment(w, g, "application/pdf", true)
			http.ServeFile(w, r, filepath.Join(s.guidesDir(), g))
			return
		}
	}
	s.renderError(w, r, http.StatusNotFound, "This guide is not available. Ask your Bicom Systems representative for it.")
}

// handleAllGuides sends every guide in one zip.
func (s *Server) handleAllGuides(w http.ResponseWriter, r *http.Request) {
	list := s.guides()
	if len(list) == 0 {
		s.renderError(w, r, http.StatusNotFound, "No guides are available yet.")
		return
	}
	attachment(w, allGuidesName, "application/zip", false)
	zw := zip.NewWriter(w)
	for _, g := range list {
		f, err := os.Open(filepath.Join(s.guidesDir(), g))
		if err != nil {
			continue
		}
		st, _ := f.Stat()
		hdr := &zip.FileHeader{Name: g, Method: zip.Deflate}
		if st != nil {
			hdr.Modified = st.ModTime()
		}
		if zf, err := zw.CreateHeader(hdr); err == nil {
			io.Copy(zf, f)
		}
		f.Close()
	}
	zw.Close()
}

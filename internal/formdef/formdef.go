// Package formdef defines the submission form (sections and items), checks
// it when admins edit it, and checks guest answers against it.
//
// Descriptions use a small markup that the browser renders: **bold**,
// [link text](https://url), lines starting with "- " as list items, and
// blank lines between paragraphs. Nothing else is interpreted, and the
// renderer escapes everything first.
package formdef

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Item types.
const (
	Short     = "short"     // one line of text
	Paragraph = "paragraph" // several lines of text
	Radio     = "radio"     // pick one option
	Checkbox  = "checkbox"  // pick any options
	File      = "file"      // file upload
	Info      = "info"      // title and description only, no answer
)

var Types = []string{Short, Paragraph, Radio, Checkbox, File, Info}

// Limits on file uploads.
const (
	MaxFilesLimit   = 20
	MaxFileMBLimit  = 2048
	MaxTotalMBLimit = 20480
	DefaultFiles    = 10
	DefaultFileMB   = 10
	DefaultTotalMB  = 100
)

// GuideLinks maps Google Drive/Docs documents that the original form linked
// to (by document ID) to the guide PDFs the portal serves instead.
var GuideLinks = map[string]string{
	"1orGL4dagr3CyZUVArNwFMHtSArhdn-i0":            "How to ship to Bicom Systems in Bosnia and Herzegovina.pdf",
	"1SK7Ni1ZwDAYxCjaTRSENthWz1uvhwnH_pm4Iv0wVz7g": "Google OAuth guide.pdf",
	"1ltCpL2D-1MeB97yTKhrxmDYBXS_jh4s8tj7YaeHy_6I": "Guide - How to publish in Chrome Web Store.pdf",
	"1g_L823jxejl5aFwsDlFVU3oJi78qzhUCUqnh3TFNjtI": "Guide - How to publish in Firefox store.pdf",
}

var googleDocRe = regexp.MustCompile(`https://(?:drive|docs)\.google\.com/[^\s()]*?/d/([A-Za-z0-9_-]+)[^\s()]*`)

// ReplaceGuideLinks points links to the known Google documents at the
// portal's own guide files (/guides/<file name>). Other links are kept.
func ReplaceGuideLinks(text string) string {
	return googleDocRe.ReplaceAllStringFunc(text, func(u string) string {
		id := googleDocRe.FindStringSubmatch(u)[1]
		if name, ok := GuideLinks[id]; ok {
			return "/guides/" + url.PathEscape(name)
		}
		return u
	})
}

// ReplaceGuideLinks in every description of the form; reports a change.
func (f *Form) ReplaceGuideLinks() bool {
	changed := false
	fix := func(s *string) {
		if n := ReplaceGuideLinks(*s); n != *s {
			*s, changed = n, true
		}
	}
	fix(&f.Description)
	for si := range f.Sections {
		fix(&f.Sections[si].Description)
		for ii := range f.Sections[si].Items {
			fix(&f.Sections[si].Items[ii].Description)
		}
	}
	return changed
}

// SVGNotes are added to the end of the upload questions' descriptions
// (by item ID): .svg versions of logos and icons are preferred as well.
var SVGNotes = map[string]string{
	"desktop_images": svgNote("the example images in the downloaded .zip file don't include them"),
	"ios_images":     svgNote("they are not among the files listed above"),
	"android_images": svgNote("they are not among the files listed above"),
	"web_images":     svgNote("they are not among the files listed above"),
}

const svgNoteMarker = "**Additional .svg files are preferred:**"

func svgNote(evenThough string) string {
	return svgNoteMarker + " please also include .svg versions of your logos and icons, even though " +
		evenThough + ". SVG files result in much better image quality."
}

// AddSVGNotes appends the SVG note to the upload questions that don't
// have it yet; reports a change.
func (f *Form) AddSVGNotes() bool {
	changed := false
	for si := range f.Sections {
		for ii := range f.Sections[si].Items {
			it := &f.Sections[si].Items[ii]
			note, ok := SVGNotes[it.ID]
			if !ok || it.Type != File || strings.Contains(it.Description, svgNoteMarker) {
				continue
			}
			it.Description = strings.TrimRight(it.Description, " \n") + "\n\n" + note
			changed = true
		}
	}
	return changed
}

// ArchiveExts are compressed archive types accepted alongside the branding
// file types, so clients can send a folder packed with any common tool.
var ArchiveExts = []string{".zip", ".7z", ".rar", ".tar", ".gz", ".tgz", ".bz2", ".xz"}

type Form struct {
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Sections    []Section `json:"sections"`
}

type Section struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Items       []Item `json:"items"`
}

type Item struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Required    bool     `json:"required"`
	Options     []string `json:"options,omitempty"`    // radio, checkbox
	AllowOther  bool     `json:"allowOther,omitempty"` // radio, checkbox: free-text "Other"
	MaxFiles    int      `json:"maxFiles,omitempty"`   // file
	MaxFileMB   int      `json:"maxFileMB,omitempty"`  // file
	MaxTotalMB  int      `json:"maxTotalMB,omitempty"` // file: all files of this question together
	Accept      []string `json:"accept,omitempty"`     // file: allowed extensions like ".zip"; empty = any
}

// Answerable reports whether an item takes an answer.
func (it Item) Answerable() bool { return it.Type != Info }

//go:embed default_form.json
var defaultForm []byte

// Default returns the starting form: a copy of the Google Form this portal replaces.
func Default() Form {
	var f Form
	if err := json.Unmarshal(defaultForm, &f); err != nil {
		panic("default form: " + err.Error())
	}
	if err := f.Validate(); err != nil {
		panic("default form: " + err.Error())
	}
	return f
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
var extRe = regexp.MustCompile(`^\.[a-z0-9]{1,10}$`)

// Validate checks an edited form and fills in defaults. IDs must be unique
// across sections and items: answers and uploads are keyed by item ID, so
// an item keeps its ID for as long as it exists.
func (f *Form) Validate() error {
	f.Title = strings.TrimSpace(f.Title)
	if f.Title == "" {
		return errors.New("the form needs a title")
	}
	if len(f.Sections) == 0 {
		return errors.New("the form needs at least one section")
	}
	seen := map[string]bool{}
	for si := range f.Sections {
		s := &f.Sections[si]
		if !idRe.MatchString(s.ID) || seen[s.ID] {
			return fmt.Errorf("section %d has a missing or duplicate ID", si+1)
		}
		seen[s.ID] = true
		s.Title = strings.TrimSpace(s.Title)
		for ii := range s.Items {
			it := &s.Items[ii]
			where := fmt.Sprintf("section %d, item %d", si+1, ii+1)
			if !idRe.MatchString(it.ID) || seen[it.ID] {
				return fmt.Errorf("%s has a missing or duplicate ID", where)
			}
			seen[it.ID] = true
			it.Title = strings.TrimSpace(it.Title)
			if it.Title == "" {
				return fmt.Errorf("%s needs a title", where)
			}
			if !slices.Contains(Types, it.Type) {
				return fmt.Errorf("%s has an unknown type %q", where, it.Type)
			}
			if it.Type == Info {
				it.Required = false
			}
			if it.Type == Radio || it.Type == Checkbox {
				var opts []string
				for _, o := range it.Options {
					if o = strings.TrimSpace(o); o != "" && !slices.Contains(opts, o) {
						opts = append(opts, o)
					}
				}
				if len(opts) == 0 && !it.AllowOther {
					return fmt.Errorf("%q needs at least one option", it.Title)
				}
				it.Options = opts
			} else {
				it.Options, it.AllowOther = nil, false
			}
			if it.Type == File {
				if it.MaxFiles <= 0 {
					it.MaxFiles = DefaultFiles
				}
				if it.MaxFileMB <= 0 {
					it.MaxFileMB = DefaultFileMB
				}
				if it.MaxTotalMB <= 0 {
					it.MaxTotalMB = max(DefaultTotalMB, it.MaxFileMB)
				}
				if it.MaxFiles > MaxFilesLimit {
					return fmt.Errorf("%q: at most %d files per question", it.Title, MaxFilesLimit)
				}
				if it.MaxFileMB > MaxFileMBLimit {
					return fmt.Errorf("%q: at most %d MB per file", it.Title, MaxFileMBLimit)
				}
				if it.MaxTotalMB > MaxTotalMBLimit {
					return fmt.Errorf("%q: at most %d MB for all files together", it.Title, MaxTotalMBLimit)
				}
				if it.MaxTotalMB < it.MaxFileMB {
					return fmt.Errorf("%q: the total size limit can't be smaller than the size limit per file", it.Title)
				}
				var acc []string
				for _, e := range it.Accept {
					e = strings.ToLower(strings.TrimSpace(e))
					if e == "" {
						continue
					}
					if !strings.HasPrefix(e, ".") {
						e = "." + e
					}
					if !extRe.MatchString(e) {
						return fmt.Errorf("%q: %q is not a file extension", it.Title, e)
					}
					if !slices.Contains(acc, e) {
						acc = append(acc, e)
					}
				}
				it.Accept = acc
			} else {
				it.MaxFiles, it.MaxFileMB, it.MaxTotalMB, it.Accept = 0, 0, 0, nil
			}
		}
	}
	return nil
}

// Item finds an item by ID.
func (f *Form) Item(id string) (Item, bool) {
	for _, s := range f.Sections {
		for _, it := range s.Items {
			if it.ID == id {
				return it, true
			}
		}
	}
	return Item{}, false
}

// Answer is a guest's answer to one item. Text is used by short and
// paragraph items, Choices by radio and checkbox items, and Other holds
// the free text of an "Other" choice.
type Answer struct {
	Text    string   `json:"text,omitempty"`
	Choices []string `json:"choices,omitempty"`
	Other   string   `json:"other,omitempty"`
}

type Answers map[string]Answer

const (
	maxShort     = 1000
	maxParagraph = 20000
)

// Clean drops answers to unknown items and trims what is kept. It is used
// for drafts, which may be incomplete, so it never fails on missing
// answers; oversize text is cut.
func (f *Form) Clean(in Answers) Answers {
	out := Answers{}
	for id, a := range in {
		it, ok := f.Item(id)
		if !ok {
			continue
		}
		switch it.Type {
		case Short:
			out[id] = Answer{Text: cut(strings.TrimSpace(strings.ReplaceAll(a.Text, "\n", " ")), maxShort)}
		case Paragraph:
			out[id] = Answer{Text: cut(a.Text, maxParagraph)}
		case Radio, Checkbox:
			var ch []string
			for _, c := range a.Choices {
				if slices.Contains(it.Options, c) && !slices.Contains(ch, c) {
					ch = append(ch, c)
				}
			}
			if it.Type == Radio && len(ch) > 1 {
				ch = ch[:1]
			}
			na := Answer{Choices: ch}
			if it.AllowOther {
				na.Other = cut(strings.TrimSpace(a.Other), maxShort)
				if it.Type == Radio && na.Other != "" && len(ch) > 0 {
					na.Other = "" // a radio choice wins over Other
				}
			}
			out[id] = na
		}
	}
	return out
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s[:n])
	return string(r[:len(r)-1])
}

// Missing lists the titles of required items that have no answer. fileCounts
// gives the number of uploaded files per file item.
func (f *Form) Missing(a Answers, fileCounts map[string]int) []string {
	var miss []string
	for _, s := range f.Sections {
		for _, it := range s.Items {
			if !it.Required {
				continue
			}
			ok := true
			switch it.Type {
			case Short, Paragraph:
				ok = strings.TrimSpace(a[it.ID].Text) != ""
			case Radio, Checkbox:
				ok = len(a[it.ID].Choices) > 0 || a[it.ID].Other != ""
			case File:
				ok = fileCounts[it.ID] > 0
			}
			if !ok {
				miss = append(miss, it.Title)
			}
		}
	}
	return miss
}

// Snapshot is what a submission keeps: the questions as they were worded
// when the guest submitted, with their answers. Later form edits never
// change it.
type Snapshot struct {
	Title    string            `json:"title"`
	Sections []SnapshotSection `json:"sections"`
}

type SnapshotSection struct {
	Title string         `json:"title"`
	Items []SnapshotItem `json:"items"`
}

type SnapshotItem struct {
	ID     string   `json:"id"`
	Type   string   `json:"type"`
	Title  string   `json:"title"`
	Answer []string `json:"answer"` // lines; for file items, the file names
}

// FileRef names a stored upload for the snapshot.
type FileRef struct {
	ItemID string
	Name   string
}

func (f *Form) Snapshot(a Answers, files []FileRef) Snapshot {
	snap := Snapshot{Title: f.Title}
	for _, s := range f.Sections {
		ss := SnapshotSection{Title: s.Title}
		for _, it := range s.Items {
			if !it.Answerable() {
				continue
			}
			si := SnapshotItem{ID: it.ID, Type: it.Type, Title: it.Title}
			switch it.Type {
			case Short, Paragraph:
				if t := strings.TrimSpace(a[it.ID].Text); t != "" {
					si.Answer = strings.Split(strings.ReplaceAll(t, "\r\n", "\n"), "\n")
				}
			case Radio, Checkbox:
				si.Answer = append(si.Answer, a[it.ID].Choices...)
				if o := a[it.ID].Other; o != "" {
					si.Answer = append(si.Answer, "Other: "+o)
				}
			case File:
				for _, fr := range files {
					if fr.ItemID == it.ID {
						si.Answer = append(si.Answer, fr.Name)
					}
				}
			}
			ss.Items = append(ss.Items, si)
		}
		if len(ss.Items) > 0 {
			snap.Sections = append(snap.Sections, ss)
		}
	}
	return snap
}

// FirstText returns the first non-empty short answer, used to label a
// submission (normally the company name).
func (s Snapshot) FirstText() string {
	for _, sec := range s.Sections {
		for _, it := range sec.Items {
			if it.Type == Short && len(it.Answer) > 0 {
				return it.Answer[0]
			}
		}
	}
	return ""
}

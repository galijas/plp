// Package export writes a submission's answers as a text file and a PDF.
// In both, each question is followed by its answer; the PDF sets answers
// in bold, the text file indents them under a marker.
package export

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"

	"plportal/internal/formdef"
)

// Meta is the header information printed above the answers.
type Meta struct {
	Email       string
	SubmittedAt time.Time // already in the display time zone
	InviteCode  string
	FilesCount  int
	FilesBytes  int64
}

const timeLayout = "2006-01-02 15:04:05 MST"

// Stamp formats a submission time for file and folder names. Colons aren't
// allowed in Windows file names, so the time uses dashes.
func Stamp(t time.Time) string { return t.Format("2006-01-02 15-04-05") }

// AnswersName and FilesName are the download names the admins asked for.
func AnswersName(email string, t time.Time, ext string) string {
	return "Form Answers - " + email + " - " + Stamp(t) + ext
}

func FilesName(email string, t time.Time) string {
	return "Uploaded Files - " + email + " - " + Stamp(t) + ".zip"
}

func WriteText(w io.Writer, snap formdef.Snapshot, m Meta) error {
	var b strings.Builder
	b.WriteString(snap.Title + "\n")
	b.WriteString(strings.Repeat("=", min(len([]rune(snap.Title)), 78)) + "\n\n")
	fmt.Fprintf(&b, "Submitted by:  %s\n", m.Email)
	fmt.Fprintf(&b, "Submitted at:  %s\n", m.SubmittedAt.Format(timeLayout))
	fmt.Fprintf(&b, "Invite code:   %s\n", m.InviteCode)
	fmt.Fprintf(&b, "Files:         %d (%s)\n", m.FilesCount, HumanBytes(m.FilesBytes))
	for _, sec := range snap.Sections {
		b.WriteString("\n\n" + sec.Title + "\n")
		b.WriteString(strings.Repeat("-", min(len([]rune(sec.Title)), 78)) + "\n")
		for _, it := range sec.Items {
			b.WriteString("\n" + it.Title + "\n")
			if len(it.Answer) == 0 {
				b.WriteString("    (no answer)\n")
				continue
			}
			for i, line := range it.Answer {
				prefix := "    "
				if i == 0 {
					prefix = "  > "
				}
				b.WriteString(prefix + line + "\n")
			}
		}
	}
	_, err := io.WriteString(w, strings.ReplaceAll(b.String(), "\n", "\r\n"))
	return err
}

//go:embed fonts/DejaVuSans.ttf
var fontRegular []byte

//go:embed fonts/DejaVuSans-Bold.ttf
var fontBold []byte

func WritePDF(w io.Writer, snap formdef.Snapshot, m Meta) error {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.AddUTF8FontFromBytes("dv", "", fontRegular)
	pdf.AddUTF8FontFromBytes("dv", "B", fontBold)
	pdf.SetMargins(20, 18, 20)
	pdf.SetAutoPageBreak(true, 18)
	pdf.SetTitle(snap.Title, true)
	pdf.SetAuthor(m.Email, true)
	pdf.SetFooterFunc(func() {
		pdf.SetY(-12)
		pdf.SetFont("dv", "", 8)
		pdf.SetTextColor(110, 118, 135)
		pdf.CellFormat(0, 5, fmt.Sprintf("%s · %s · page %d", m.Email, Stamp(m.SubmittedAt), pdf.PageNo()), "", 0, "R", false, 0, "")
	})
	pdf.AddPage()
	width, _ := pdf.GetPageSize()
	textW := width - 40

	ink := func() { pdf.SetTextColor(22, 30, 48) }
	muted := func() { pdf.SetTextColor(95, 104, 122) }

	pdf.SetFont("dv", "B", 17)
	ink()
	pdf.MultiCell(textW, 8, snap.Title, "", "L", false)
	pdf.Ln(3)

	pdf.SetFont("dv", "", 9.5)
	meta := [][2]string{
		{"Submitted by", m.Email},
		{"Submitted at", m.SubmittedAt.Format(timeLayout)},
		{"Invite code", m.InviteCode},
		{"Files", fmt.Sprintf("%d (%s)", m.FilesCount, HumanBytes(m.FilesBytes))},
	}
	for _, kv := range meta {
		muted()
		pdf.CellFormat(30, 5.5, kv[0], "", 0, "L", false, 0, "")
		ink()
		pdf.CellFormat(0, 5.5, kv[1], "", 1, "L", false, 0, "")
	}

	for _, sec := range snap.Sections {
		pdf.Ln(6)
		if pdf.GetY() > 250 {
			pdf.AddPage()
		}
		pdf.SetFont("dv", "B", 13)
		ink()
		pdf.MultiCell(textW, 7, sec.Title, "", "L", false)
		y := pdf.GetY() + 1
		pdf.SetDrawColor(0, 137, 199)
		pdf.SetLineWidth(0.5)
		pdf.Line(20, y, 20+textW, y)
		pdf.Ln(3)

		for _, it := range sec.Items {
			if pdf.GetY() > 262 {
				pdf.AddPage()
			}
			pdf.Ln(2.5)
			pdf.SetFont("dv", "", 9.5)
			muted()
			pdf.MultiCell(textW, 5, it.Title, "", "L", false)
			pdf.Ln(0.8)
			top := pdf.GetY()
			pdf.SetX(24)
			if len(it.Answer) == 0 {
				pdf.SetFont("dv", "", 10.5)
				muted()
				pdf.MultiCell(textW-4, 5.6, "(no answer)", "", "L", false)
			} else {
				pdf.SetFont("dv", "B", 10.5)
				ink()
				for _, line := range it.Answer {
					pdf.SetX(24)
					if line == "" {
						line = " "
					}
					pdf.MultiCell(textW-4, 5.6, line, "", "L", false)
				}
			}
			// Answer bar, only when the answer stayed on one page.
			if bottom := pdf.GetY(); bottom > top {
				pdf.SetDrawColor(0, 137, 199)
				pdf.SetLineWidth(0.8)
				pdf.Line(21, top+0.6, 21, bottom-0.6)
			}
		}
	}
	if err := pdf.Error(); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

package server

import (
	"testing"
	"time"

	"plportal/internal/export"
)

func TestCleanFileName(t *testing.T) {
	cases := map[string]string{
		`C:\fakepath\logo.png`:   "logo.png",
		"../../etc/passwd":       "passwd",
		"a<b>:c?.zip":            "a_b__c_.zip",
		"  ..  ":                 "file",
		"name\x00with\nctrl.txt": "name_with_ctrl.txt",
	}
	for in, want := range cases {
		if got := cleanFileName(in); got != want {
			t.Errorf("cleanFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExportNames(t *testing.T) {
	at := time.Date(2026, 10, 6, 14, 30, 5, 0, time.UTC)
	if got := export.AnswersName("a@b.com", at, ".pdf"); got != "Form Answers - a@b.com - 2026-10-06 14-30-05.pdf" {
		t.Error(got)
	}
	if got := export.FilesName("a@b.com", at); got != "Uploaded Files - a@b.com - 2026-10-06 14-30-05.zip" {
		t.Error(got)
	}
	if got := folderName("a@b.com", at); got != "a@b.com - 2026-10-06 14-30-05" {
		t.Error(got)
	}
}

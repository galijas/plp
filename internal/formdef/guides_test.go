package formdef

import (
	"strings"
	"testing"
)

func TestReplaceGuideLinks(t *testing.T) {
	in := "see [URL](https://drive.google.com/file/d/1orGL4dagr3CyZUVArNwFMHtSArhdn-i0/view?usp=sharing) and " +
		"[guide](https://docs.google.com/document/d/1g_L823jxejl5aFwsDlFVU3oJi78qzhUCUqnh3TFNjtI/edit?tab=t.0) and " +
		"[other](https://docs.google.com/document/d/UNKNOWN123/edit) and [ms](https://msdn.microsoft.com/a(v=vs.85).aspx)"
	out := ReplaceGuideLinks(in)
	for _, want := range []string{
		"[URL](/guides/How%20to%20ship%20to%20Bicom%20Systems%20in%20Bosnia%20and%20Herzegovina.pdf)",
		"[guide](/guides/Guide%20-%20How%20to%20publish%20in%20Firefox%20store.pdf)",
		"[other](https://docs.google.com/document/d/UNKNOWN123/edit)",
		"[ms](https://msdn.microsoft.com/a(v=vs.85).aspx)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestDefaultFormHasNoGoogleDocLinks(t *testing.T) {
	f := Default()
	if f.ReplaceGuideLinks() {
		t.Error("the default form still links to Google Drive/Docs guides")
	}
}

func TestDefaultFormHasSVGNotes(t *testing.T) {
	f := Default()
	if f.AddSVGNotes() {
		t.Error("the default form is missing the .svg notes")
	}
}

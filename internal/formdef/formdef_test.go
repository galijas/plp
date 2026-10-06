package formdef

import "testing"

func TestDefaultFormIsValid(t *testing.T) {
	f := Default()
	if len(f.Sections) != 3 {
		t.Fatalf("sections = %d, want 3", len(f.Sections))
	}
	it, ok := f.Item("code_sign")
	if !ok || it.Type != Radio || len(it.Options) != 3 || !it.Required {
		t.Fatalf("code_sign = %+v", it)
	}
}

func TestValidateRejectsDuplicateIDs(t *testing.T) {
	f := Form{Title: "t", Sections: []Section{{ID: "s", Items: []Item{
		{ID: "a", Type: Short, Title: "A"}, {ID: "a", Type: Short, Title: "B"},
	}}}}
	if err := f.Validate(); err == nil {
		t.Fatal("duplicate IDs accepted")
	}
}

func TestCleanDropsUnknownAndInvalidChoices(t *testing.T) {
	f := Default()
	a := f.Clean(Answers{
		"nope":         {Text: "x"},
		"code_sign":    {Choices: []string{"made up", f.Sections[1].Items[3].Options[1], f.Sections[1].Items[3].Options[2]}},
		"company_name": {Text: "  ACME\nCorp  "},
	})
	if _, ok := a["nope"]; ok {
		t.Error("unknown item kept")
	}
	if got := a["code_sign"].Choices; len(got) != 1 {
		t.Errorf("radio choices = %v, want exactly one valid choice", got)
	}
	if got := a["company_name"].Text; got != "ACME Corp" {
		t.Errorf("short text = %q", got)
	}
}

func TestMissingCountsFiles(t *testing.T) {
	f := Form{Title: "t", Sections: []Section{{ID: "s", Items: []Item{
		{ID: "f", Type: File, Title: "Logo", Required: true},
		{ID: "n", Type: Short, Title: "Name", Required: true},
		{ID: "i", Type: Info, Title: "Read me"},
	}}}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if m := f.Missing(Answers{"n": {Text: "x"}}, nil); len(m) != 1 || m[0] != "Logo" {
		t.Errorf("missing = %v", m)
	}
	if m := f.Missing(Answers{"n": {Text: "x"}}, map[string]int{"f": 1}); len(m) != 0 {
		t.Errorf("missing = %v", m)
	}
}

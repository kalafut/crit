package vcs

import (
	"reflect"
	"testing"
)

func TestParseFossilChanges(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []FileChange
	}{
		{name: "empty", in: "", want: nil},
		{
			name: "fossil changes --differ",
			in:   "EDITED     f.txt\nEXTRA      sub/g.go\nADDED      new.txt\nDELETED    old.txt\nMISSING    gone.txt\n",
			want: []FileChange{
				{Path: "f.txt", Status: "modified"},
				{Path: "sub/g.go", Status: "untracked"},
				{Path: "new.txt", Status: "added"},
				{Path: "old.txt", Status: "deleted"},
				{Path: "gone.txt", Status: "deleted"},
			},
		},
		{
			name: "fossil diff --brief (variable spacing)",
			in:   "CHANGED  f.txt\nADDED    new.txt\nDELETED  sub/g.go\n",
			want: []FileChange{
				{Path: "f.txt", Status: "modified"},
				{Path: "new.txt", Status: "added"},
				{Path: "sub/g.go", Status: "deleted"},
			},
		},
		{
			name: "rename arrow",
			in:   "EDITED     f.txt  ->  f2.txt\nRENAMED    a b.txt  ->  c d.txt\n",
			want: []FileChange{
				{Path: "f2.txt", OldPath: "f.txt", Status: "renamed"},
				{Path: "c d.txt", OldPath: "a b.txt", Status: "renamed"},
			},
		},
		{
			name: "path with spaces and unknown status words skipped",
			in:   "EDITED     dir/my file.txt\nMOVED_FILE /abs/path\nNOT_A_FILE weird\n",
			want: []FileChange{{Path: "dir/my file.txt", Status: "modified"}},
		},
		{
			name: "CRLF and blank lines",
			in:   "EDITED  a.txt\r\n\r\nADDED  b.txt\r\n",
			want: []FileChange{
				{Path: "a.txt", Status: "modified"},
				{Path: "b.txt", Status: "added"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseFossilChanges(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseFossilChanges() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseFossilNumstat(t *testing.T) {
	in := "  INSERTED    DELETED\n         3          1 f.txt\n         1          0 dir/my file.txt\n         4          1 TOTAL over 2 changed files\n"
	want := map[string]NumstatEntry{
		"f.txt":           {Additions: 3, Deletions: 1},
		"dir/my file.txt": {Additions: 1, Deletions: 0},
	}
	if got := parseFossilNumstat(in); !reflect.DeepEqual(got, want) {
		t.Errorf("parseFossilNumstat() = %+v, want %+v", got, want)
	}
	if got := parseFossilNumstat(""); len(got) != 0 {
		t.Errorf("empty input: got %+v", got)
	}
}

func TestParseFossilTimeline(t *testing.T) {
	in := "" +
		"8b1b468107fa5f291bd40d8f8c05e16dfff5f10c60d408c3a94d512eaddf6b39\n8b1b468107\nkalafut\n2026-09-20 02:36:51\nfeature work\n" +
		"6aee1204f294901c334cc55fb93a641093654fb19584ffe70fea8b128642a06d\n6aee1204f2\nkalafut\n2026-09-20 02:30:00\n\n" +
		"+++ end of timeline (2) +++\n"
	want := []CommitInfo{
		{SHA: "8b1b468107fa5f291bd40d8f8c05e16dfff5f10c60d408c3a94d512eaddf6b39", ShortSHA: "8b1b468107", Author: "kalafut", Date: "2026-09-20 02:36:51", Message: "feature work"},
		{SHA: "6aee1204f294901c334cc55fb93a641093654fb19584ffe70fea8b128642a06d", ShortSHA: "6aee1204f2", Author: "kalafut", Date: "2026-09-20 02:30:00", Message: ""},
	}
	if got := parseFossilTimeline(in); !reflect.DeepEqual(got, want) {
		t.Errorf("parseFossilTimeline() = %+v, want %+v", got, want)
	}
	if got := parseFossilTimeline("+++ no more data (0) +++\n"); got != nil {
		t.Errorf("trailer only: got %+v, want nil", got)
	}
}

func TestParseFossilMergeBase(t *testing.T) {
	tests := []struct{ in, want string }{
		{"pivot=6aee1204f294901c334cc55fb93a641093654fb19584ffe70fea8b128642a06d\n", "6aee1204f294901c334cc55fb93a641093654fb19584ffe70fea8b128642a06d"},
		{"abc123\n", "abc123"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := parseFossilMergeBase(tt.in); got != tt.want {
			t.Errorf("parseFossilMergeBase(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseFossilBranchList(t *testing.T) {
	got := parseFossilBranchList(" * feature\n   trunk\n")
	want := []string{"feature", "trunk"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseFossilBranchList() = %v, want %v", got, want)
	}
}

func TestFossilRef(t *testing.T) {
	tests := []struct{ in, want string }{
		{"HEAD", "current"},
		{"trunk", "trunk"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := fossilRef(tt.in); got != tt.want {
			t.Errorf("fossilRef(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseUnifiedDiff_FossilPreamble(t *testing.T) {
	// Fossil prefixes each file with "Index:" and a rule line; the parser
	// must ignore them and still pick up the hunk.
	in := "Index: f.txt\n==================================================================\n--- f.txt\t\n+++ f.txt\t\n@@ -1,3 +1,4 @@\n a\n-b\n+B\n c\n+d\n"
	hunks := ParseUnifiedDiff(in)
	if len(hunks) != 1 {
		t.Fatalf("hunks = %d, want 1", len(hunks))
	}
	if hunks[0].OldStart != 1 || hunks[0].NewCount != 4 || len(hunks[0].Lines) != 5 {
		t.Errorf("hunk = %+v", hunks[0])
	}
}

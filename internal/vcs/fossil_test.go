package vcs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fossilFixture is a throwaway Fossil checkout with:
//
//	trunk:   f.txt (a b c), sub/g.go (x)
//	feature: f.txt (a B c d), new.txt added, sub/g.go deleted
//	working: f.txt gets a fifth line "e"; u.txt is untracked
type fossilFixture struct {
	dir       string
	trunkSHA  string
	branchSHA string
}

func initTestFossilRepo(t *testing.T) fossilFixture {
	t.Helper()
	if _, err := exec.LookPath("fossil"); err != nil {
		t.Skip("fossil not installed")
	}
	base := t.TempDir()
	// Keep the developer's ~/.fossil global settings out of the test.
	t.Setenv("FOSSIL_HOME", base)
	repo := filepath.Join(base, "repo.fossil")
	dir := filepath.Join(base, "co")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runFossil(t, base, "init", "-A", "tester", repo)
	runFossil(t, dir, "open", repo)
	runFossil(t, dir, "user", "default", "tester")
	runFossil(t, dir, "settings", "mv-rm-files", "on")

	writeFile(t, filepath.Join(dir, "f.txt"), "a\nb\nc\n")
	writeFile(t, filepath.Join(dir, "sub/g.go"), "x\n")
	runFossil(t, dir, "add", "f.txt", "sub/g.go")
	runFossil(t, dir, "commit", "-m", "init", "--no-warnings")
	trunkSHA := strings.TrimSpace(mustFossilResolve(t, dir, "trunk"))

	runFossil(t, dir, "branch", "new", "feature", "trunk")
	runFossil(t, dir, "update", "feature")
	writeFile(t, filepath.Join(dir, "f.txt"), "a\nB\nc\nd\n")
	writeFile(t, filepath.Join(dir, "new.txt"), "new\n")
	runFossil(t, dir, "add", "new.txt")
	runFossil(t, dir, "rm", "sub/g.go")
	runFossil(t, dir, "commit", "-m", "feature work", "--no-warnings")
	branchSHA := strings.TrimSpace(mustFossilResolve(t, dir, "feature"))

	writeFile(t, filepath.Join(dir, "f.txt"), "a\nB\nc\nd\ne\n")
	writeFile(t, filepath.Join(dir, "u.txt"), "untracked\n")
	return fossilFixture{dir: dir, trunkSHA: trunkSHA, branchSHA: branchSHA}
}

func runFossil(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("fossil", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

func mustFossilResolve(t *testing.T, dir, name string) string {
	t.Helper()
	sha, err := FossilResolve(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func statusByPath(changes []FileChange) map[string]string {
	m := make(map[string]string, len(changes))
	for _, c := range changes {
		m[c.Path] = c.Status
	}
	return m
}

func TestFossilDetect(t *testing.T) {
	fx := initTestFossilRepo(t)
	t.Chdir(fx.dir)

	if root, ok := fossilCheckoutRootFromTest(filepath.Join(fx.dir, "sub")); !ok || root != fx.dir {
		t.Errorf("fossilCheckoutRootFrom(sub) = %q,%v want %q,true", root, ok, fx.dir)
	}
	if _, ok := fossilCheckoutRootFromTest(t.TempDir()); ok {
		t.Error("fossilCheckoutRootFrom(empty temp) = true, want false")
	}

	v := DetectVCS("")
	if v == nil || v.Name() != "fossil" {
		t.Fatalf("auto-detect = %v, want fossil", v)
	}
	if v := DetectVCS("fossil"); v == nil || v.Name() != "fossil" {
		t.Fatalf("DetectVCS(fossil) = %v", v)
	}
}

func TestFossilVCS_Basics(t *testing.T) {
	fx := initTestFossilRepo(t)
	t.Chdir(fx.dir)
	f := &FossilVCS{}

	root, err := f.RepoRoot()
	if err != nil || root != fx.dir {
		t.Errorf("RepoRoot = %q, %v; want %q", root, err, fx.dir)
	}
	if got := f.CurrentBranch(); got != "feature" {
		t.Errorf("CurrentBranch = %q, want feature", got)
	}
	if got := f.DefaultBranch(); got != "trunk" {
		t.Errorf("DefaultBranch = %q, want trunk", got)
	}
	if got := f.DefaultBaseRef(); got != "trunk" {
		t.Errorf("DefaultBaseRef = %q, want trunk", got)
	}
	f.SetDefaultBranchOverride("feature")
	if got := f.DefaultBranch(); got != "feature" {
		t.Errorf("override DefaultBranch = %q, want feature", got)
	}
	f.SetDefaultBranchOverride("")
	if got := f.UserName(); got != "tester" {
		t.Errorf("UserName = %q, want tester", got)
	}
	if f.HasStagingArea() {
		t.Error("HasStagingArea = true, want false")
	}

	mb, err := f.MergeBase("trunk")
	if err != nil || mb != fx.trunkSHA {
		t.Errorf("MergeBase(trunk) = %q, %v; want %q", mb, err, fx.trunkSHA)
	}
	mb, err = f.MergeBaseOf("feature", "trunk", fx.dir)
	if err != nil || mb != fx.trunkSHA {
		t.Errorf("MergeBaseOf = %q, %v; want %q", mb, err, fx.trunkSHA)
	}
	if _, err := f.MergeBaseOf("nosuch", "trunk", fx.dir); err == nil {
		t.Error("MergeBaseOf(nosuch) succeeded, want error")
	}
}

func TestFossilVCS_ChangedFiles(t *testing.T) {
	fx := initTestFossilRepo(t)
	f := &FossilVCS{}

	got, err := f.ChangedFilesFromBaseInDir("trunk", fx.dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"f.txt": "modified", "new.txt": "added", "sub/g.go": "deleted", "u.txt": "untracked"}
	if s := statusByPath(got); len(s) != len(want) || s["f.txt"] != "modified" || s["new.txt"] != "added" || s["sub/g.go"] != "deleted" || s["u.txt"] != "untracked" {
		t.Errorf("ChangedFilesFromBaseInDir = %v, want %v", s, want)
	}

	// A `fossil rm` with mv-rm-files off leaves the file on disk; it must
	// appear once (deleted), not also as untracked.
	runFossil(t, fx.dir, "settings", "mv-rm-files", "off")
	writeFile(t, filepath.Join(fx.dir, "sub/g.go"), "x\n")
	got, err = f.ChangedFilesFromBaseInDir("trunk", fx.dir)
	if err != nil {
		t.Fatal(err)
	}
	var gCount int
	for _, c := range got {
		if c.Path == "sub/g.go" {
			gCount++
			if c.Status != "deleted" {
				t.Errorf("sub/g.go status = %q, want deleted", c.Status)
			}
		}
	}
	if gCount != 1 {
		t.Errorf("sub/g.go listed %d times, want 1", gCount)
	}
	if err := os.Remove(filepath.Join(fx.dir, "sub/g.go")); err != nil {
		t.Fatal(err)
	}

	got, err = f.ChangedFilesOnDefaultInDir(fx.dir)
	if err != nil {
		t.Fatal(err)
	}
	if s := statusByPath(got); s["f.txt"] != "modified" || s["u.txt"] != "untracked" || len(s) != 2 {
		t.Errorf("ChangedFilesOnDefaultInDir = %v", s)
	}

	got, err = f.ChangedFilesForCommit(fx.branchSHA, fx.dir)
	if err != nil {
		t.Fatal(err)
	}
	if s := statusByPath(got); s["f.txt"] != "modified" || s["new.txt"] != "added" || s["sub/g.go"] != "deleted" || len(s) != 3 {
		t.Errorf("ChangedFilesForCommit = %v", s)
	}

	got, err = f.ChangedFilesBetweenSHAs(fx.trunkSHA, fx.branchSHA, fx.dir)
	if err != nil {
		t.Fatal(err)
	}
	if s := statusByPath(got); len(s) != 3 || s["u.txt"] != "" {
		t.Errorf("ChangedFilesBetweenSHAs = %v (must exclude untracked)", s)
	}

	if got, _ := f.ChangedFilesScoped("staged", "trunk"); got != nil {
		t.Errorf("staged scope = %v, want nil", got)
	}

	untracked, err := f.UntrackedFiles(fx.dir)
	if err != nil || len(untracked) != 1 || untracked[0].Path != "u.txt" || untracked[0].Status != "untracked" {
		t.Errorf("UntrackedFiles = %+v, %v", untracked, err)
	}
	all, err := f.AllTrackedFiles(fx.dir)
	if err != nil {
		t.Fatal(err)
	}
	joined := " " + strings.Join(all, " ") + " "
	for _, p := range []string{"f.txt", "new.txt", "u.txt"} {
		if !strings.Contains(joined, " "+p+" ") {
			t.Errorf("AllTrackedFiles missing %s: %v", p, all)
		}
	}

	for path, want := range map[string]string{"f.txt": "modified", "new.txt": "added", "sub/g.go": "deleted", "u.txt": "untracked"} {
		if got := f.FileStatusInRepo(path, "trunk", fx.dir); got != want {
			t.Errorf("FileStatusInRepo(%s) = %q, want %q", path, got, want)
		}
	}
	if got := f.FileStatusInRepo("f.txt", "", fx.dir); got != "modified" {
		t.Errorf("FileStatusInRepo(empty base) = %q", got)
	}
	if f.WorkingTreeFingerprint() == "" {
		t.Chdir(fx.dir)
		if f.WorkingTreeFingerprint() == "" {
			t.Error("WorkingTreeFingerprint empty in dirty checkout")
		}
	}
}

func TestFossilVCS_Diffs(t *testing.T) {
	fx := initTestFossilRepo(t)
	f := &FossilVCS{}

	hunks, err := f.FileDiffUnified("f.txt", "trunk", fx.dir, false)
	if err != nil || len(hunks) != 1 {
		t.Fatalf("FileDiffUnified = %+v, %v", hunks, err)
	}
	var adds, dels int
	for _, l := range hunks[0].Lines {
		switch l.Type {
		case "add":
			adds++
		case "del":
			dels++
		}
	}
	if adds != 3 || dels != 1 {
		t.Errorf("f.txt vs trunk: adds=%d dels=%d, want 3/1", adds, dels)
	}

	// "HEAD" must be translated to the current checkout.
	hunks, err = f.FileDiffUnified("f.txt", "HEAD", fx.dir, false)
	if err != nil || len(hunks) != 1 || len(hunks[0].Lines) == 0 {
		t.Fatalf("FileDiffUnified(HEAD) = %+v, %v", hunks, err)
	}
	// Empty base diffs the working checkout against its check-in.
	hunks, err = f.FileDiffUnified("f.txt", "", fx.dir, false)
	if err != nil || len(hunks) != 1 {
		t.Fatalf("FileDiffUnified(empty base) = %+v, %v", hunks, err)
	}

	// Added file gets full hunks thanks to -N.
	hunks, err = f.FileDiffUnified("new.txt", "trunk", fx.dir, false)
	if err != nil || len(hunks) != 1 || len(hunks[0].Lines) != 1 || hunks[0].Lines[0].Type != "add" {
		t.Errorf("FileDiffUnified(new.txt) = %+v, %v", hunks, err)
	}

	// Whitespace-only edit collapses with ignoreWhitespace.
	writeFile(t, filepath.Join(fx.dir, "new.txt"), "new   \n")
	hunks, _ = f.FileDiffUnified("new.txt", "feature", fx.dir, false)
	if len(hunks) != 1 {
		t.Errorf("whitespace change not detected: %+v", hunks)
	}
	hunks, _ = f.FileDiffUnified("new.txt", "feature", fx.dir, true)
	if len(hunks) != 0 {
		t.Errorf("ignoreWhitespace still produced hunks: %+v", hunks)
	}

	hunks, err = f.FileDiffForCommit("f.txt", fx.branchSHA, fx.dir, false)
	if err != nil || len(hunks) != 1 {
		t.Fatalf("FileDiffForCommit = %+v, %v", hunks, err)
	}
	hunks, err = f.FileDiffBetweenSHAs("f.txt", "", fx.trunkSHA, fx.branchSHA, fx.dir, false)
	if err != nil || len(hunks) != 1 {
		t.Fatalf("FileDiffBetweenSHAs = %+v, %v", hunks, err)
	}
	hunks, err = f.FileDiffScoped("f.txt", "staged", "trunk", fx.dir, false)
	if err != nil || hunks != nil {
		t.Errorf("FileDiffScoped(staged) = %+v, %v; want nil", hunks, err)
	}
	if _, err := f.FileDiffUnified("f.txt", "nosuch", fx.dir, false); err == nil {
		t.Error("FileDiffUnified(nosuch) succeeded, want error")
	}

	ns, err := f.DiffNumstat("trunk", fx.dir)
	if err != nil {
		t.Fatal(err)
	}
	if ns["f.txt"] != (NumstatEntry{Additions: 3, Deletions: 1}) {
		t.Errorf("DiffNumstat f.txt = %+v", ns["f.txt"])
	}
	ns, err = f.DiffNumstatBetweenSHAs(fx.trunkSHA, fx.branchSHA, fx.dir)
	if err != nil {
		t.Fatal(err)
	}
	if ns["f.txt"] != (NumstatEntry{Additions: 2, Deletions: 1}) || ns["new.txt"] != (NumstatEntry{Additions: 1}) {
		t.Errorf("DiffNumstatBetweenSHAs = %+v", ns)
	}
	if _, err := f.DiffNumstatBetweenSHAs("", fx.branchSHA, fx.dir); err == nil {
		t.Error("DiffNumstatBetweenSHAs with empty base succeeded")
	}
}

func TestFossilVCS_ContentAndHistory(t *testing.T) {
	fx := initTestFossilRepo(t)
	f := &FossilVCS{}

	content, err := f.FileContentAtRef("f.txt", "trunk", fx.dir)
	if err != nil || content != "a\nb\nc\n" {
		t.Errorf("FileContentAtRef = %q, %v", content, err)
	}
	if c, err := f.FileContentAtRef("f.txt", "", fx.dir); err != nil || c != "" {
		t.Errorf("FileContentAtRef(empty ref) = %q, %v", c, err)
	}

	data, err := f.ReadFileAtSHA(fx.trunkSHA, "f.txt", fx.dir)
	if err != nil || string(data) != "a\nb\nc\n" {
		t.Errorf("ReadFileAtSHA = %q, %v", data, err)
	}
	data, err = f.ReadFileAtSHA(fx.trunkSHA, "new.txt", fx.dir)
	if err != nil || data != nil {
		t.Errorf("ReadFileAtSHA(missing path) = %q, %v; want nil, nil", data, err)
	}
	if _, err := f.ReadFileAtSHA("nosuchref", "f.txt", fx.dir); err == nil {
		t.Error("ReadFileAtSHA(bad ref) succeeded, want error")
	}

	if !f.HasObject(fx.trunkSHA, fx.dir) || !f.HasObject(fx.trunkSHA[:10], fx.dir) {
		t.Error("HasObject(trunk sha) = false")
	}
	if f.HasObject("deadbeefdeadbeef", fx.dir) {
		t.Error("HasObject(bogus) = true")
	}

	log, err := f.CommitLog("trunk", "", fx.dir)
	if err != nil {
		t.Fatal(err)
	}
	// feature has two check-ins beyond trunk: the branch-creation and "feature work".
	if len(log) != 2 || log[0].SHA != fx.branchSHA || log[0].Message != "feature work" || log[0].Author != "tester" || log[0].ShortSHA == "" || log[0].Date == "" {
		t.Errorf("CommitLog = %+v", log)
	}
	log, err = f.CommitLog("trunk", fx.branchSHA, fx.dir)
	if err != nil || len(log) != 2 {
		t.Errorf("CommitLog(explicit head) = %+v, %v", log, err)
	}
	if log, _ := f.CommitLog("", "", fx.dir); log != nil {
		t.Errorf("CommitLog(empty base) = %+v, want nil", log)
	}

	if got, err := f.RemoteBranches(fx.dir); err != nil || got != nil {
		t.Errorf("RemoteBranches = %v, %v; want nil", got, err)
	}
	branches, err := f.Branches(fx.dir)
	if err != nil || len(branches) != 2 {
		t.Errorf("Branches = %v, %v", branches, err)
	}
}

func TestFossilStackHelpers(t *testing.T) {
	fx := initTestFossilRepo(t)
	t.Chdir(fx.dir)
	v := DetectVCS("fossil")

	sha, err := ResolveCommitOID(v, "feature", fx.dir)
	if err != nil || sha != fx.branchSHA {
		t.Errorf("ResolveCommitOID = %q, %v; want %q", sha, err, fx.branchSHA)
	}
	if _, err := ResolveCommitOID(v, "nosuch", fx.dir); err == nil {
		t.Error("ResolveCommitOID(nosuch) succeeded")
	}
	sha, err = ResolveDefaultBranchSHA(v, fx.dir, "trunk")
	if err != nil || sha != fx.trunkSHA {
		t.Errorf("ResolveDefaultBranchSHA = %q, %v; want %q", sha, err, fx.trunkSHA)
	}

	anc, err := WalkAncestors(v, fx.dir, 10)
	if err != nil || len(anc) < 3 || anc[0] != fx.branchSHA {
		t.Errorf("WalkAncestors = %v, %v", anc, err)
	}
	anc, err = WalkAncestors(v, fx.dir, 1)
	if err != nil || len(anc) != 1 {
		t.Errorf("WalkAncestors(depth 1) = %v, %v", anc, err)
	}

	chain := TopicChainSHAs(v, fx.dir)
	if !chain[fx.branchSHA] || chain[fx.trunkSHA] || len(chain) != 2 {
		t.Errorf("TopicChainSHAs = %v", chain)
	}
	if got := CommitSubjectFor(v, fx.dir, fx.branchSHA); got != "feature work" {
		t.Errorf("CommitSubjectFor = %q", got)
	}

	tips, err := LocalBranchTips(v, fx.dir)
	if err != nil || tips[fx.branchSHA] != "feature" || tips[fx.trunkSHA] != "trunk" {
		t.Errorf("LocalBranchTips = %v, %v", tips, err)
	}
	remote, err := RemoteBranchTips(v, fx.dir, "trunk")
	if err != nil || remote != nil {
		t.Errorf("RemoteBranchTips = %v, %v; want nil", remote, err)
	}

	targets, err := CompareTargetsFor(v, fx.dir)
	if err != nil || targets.VCS != "fossil" || targets.Detected != "trunk" || len(targets.Local) != 2 || targets.Remote != nil {
		t.Errorf("CompareTargetsFor = %+v, %v", targets, err)
	}
}

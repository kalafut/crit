package vcs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// FossilVCS implements VCS for Fossil SCM checkouts.
//
// Fossil differences that shape this backend:
//   - There is no staging area; "staged"/"unstaged" scopes return nothing.
//   - Branches are repository-global (autosync), so there are no
//     remote-tracking refs; RemoteBranches returns nil and the compare
//     picker lists `fossil branch list` as local targets.
//   - The default branch is "trunk".
//   - The working checkout is addressed as "current"; the git literal
//     "HEAD" that some callers pass is translated by fossilRef.
//   - Check-in hashes are SHA3-256 (64 hex chars).
type FossilVCS struct {
	defaultBranchOnce sync.Once
	defaultBranch     string
	overrideBranch    string
	defaultBranchMu   sync.RWMutex
}

// fossilCheckoutMarkers are the files Fossil writes at a checkout root.
var fossilCheckoutMarkers = []string{".fslckout", "_FOSSIL_"}

func (f *FossilVCS) Name() string { return "fossil" }

// fossilRef translates git-flavoured refs callers may pass into Fossil names.
func fossilRef(ref string) string {
	if ref == "HEAD" {
		return "current"
	}
	return ref
}

// RepoRoot returns the checkout root by walking up from cwd looking for a
// checkout marker file. Falls back to `fossil info` when no marker is found
// (e.g. an unusual checkout layout).
func (f *FossilVCS) RepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if root, ok := fossilCheckoutRootFrom(cwd); ok {
		return root, nil
	}
	out, err := exec.Command("fossil", "info").Output()
	if err != nil {
		return "", fmt.Errorf("fossil info: %w", err)
	}
	for _, line := range SplitNonEmpty(string(out)) {
		if rest, ok := strings.CutPrefix(line, "local-root:"); ok {
			return filepath.Clean(strings.TrimSpace(rest)), nil
		}
	}
	return "", errors.New("fossil info: no local-root in output")
}

// fossilCheckoutRootFrom walks up from dir looking for a Fossil checkout
// marker file and returns the directory that holds it.
func fossilCheckoutRootFrom(dir string) (string, bool) {
	for {
		for _, marker := range fossilCheckoutMarkers {
			if info, err := os.Stat(filepath.Join(dir, marker)); err == nil && !info.IsDir() {
				return dir, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// hasFossilCheckout reports whether cwd is inside a Fossil checkout.
func hasFossilCheckout() bool {
	dir, err := os.Getwd()
	if err != nil {
		return false
	}
	_, ok := fossilCheckoutRootFrom(dir)
	return ok
}

// CurrentBranch returns the branch of the current checkout.
func (f *FossilVCS) CurrentBranch() string {
	out, err := exec.Command("fossil", "branch", "current").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// DefaultBranch returns the cached default branch, detecting it on first call.
// If an override is set, it is returned immediately without caching.
func (f *FossilVCS) DefaultBranch() string {
	f.defaultBranchMu.RLock()
	override := f.overrideBranch
	f.defaultBranchMu.RUnlock()
	if override != "" {
		return override
	}
	f.defaultBranchOnce.Do(func() {
		f.defaultBranchMu.Lock()
		if f.overrideBranch == "" {
			f.defaultBranch = detectFossilDefaultBranch()
		}
		f.defaultBranchMu.Unlock()
	})
	f.defaultBranchMu.RLock()
	defer f.defaultBranchMu.RUnlock()
	if f.overrideBranch != "" {
		return f.overrideBranch
	}
	return f.defaultBranch
}

// SetDefaultBranchOverride overrides the default branch detection.
func (f *FossilVCS) SetDefaultBranchOverride(branch string) {
	f.defaultBranchMu.Lock()
	f.overrideBranch = branch
	f.defaultBranchMu.Unlock()
}

// GetDefaultBranchOverride returns the current default branch override, if any.
func (f *FossilVCS) GetDefaultBranchOverride() string {
	f.defaultBranchMu.RLock()
	defer f.defaultBranchMu.RUnlock()
	return f.overrideBranch
}

// DefaultBaseRef returns the default branch as-is; Fossil has no
// origin/<branch>-style remote tracking refs.
func (f *FossilVCS) DefaultBaseRef() string { return f.DefaultBranch() }

// MergeBase returns the common ancestor between the current checkout and ref.
func (f *FossilVCS) MergeBase(ref string) (string, error) {
	return f.MergeBaseOf("current", ref, "")
}

// MergeBaseOf returns the common ancestor of two arbitrary check-ins.
func (f *FossilVCS) MergeBaseOf(a, b, dir string) (string, error) {
	out, err := FossilCommandInDir(dir, "merge-base", fossilRef(a), fossilRef(b))
	if err != nil {
		return "", err
	}
	hash := parseFossilMergeBase(out)
	if hash == "" {
		return "", fmt.Errorf("fossil merge-base %s %s: no pivot", a, b)
	}
	return hash, nil
}

// ChangedFilesOnDefaultInDir returns working-checkout changes (edited, added,
// deleted, and untracked "EXTRA" files).
func (f *FossilVCS) ChangedFilesOnDefaultInDir(dir string) ([]FileChange, error) {
	out, err := FossilCommandInDir(dir, "changes", "--differ")
	if err != nil {
		return nil, err
	}
	return parseFossilChanges(out), nil
}

// ChangedFilesFromBaseInDir returns files that differ between baseRef and the
// working checkout, plus untracked files.
func (f *FossilVCS) ChangedFilesFromBaseInDir(baseRef, dir string) ([]FileChange, error) {
	if baseRef == "" {
		return f.ChangedFilesOnDefaultInDir(dir)
	}
	out, err := FossilCommandInDir(dir, "diff", "-i", "--brief", "--from", fossilRef(baseRef))
	if err != nil {
		return nil, err
	}
	changes := parseFossilChanges(out)
	untracked, err := f.UntrackedFiles(dir)
	if err != nil {
		return changes, nil //nolint:nilerr // graceful: tracked changes are still useful
	}
	// `fossil rm` leaves the file on disk unless mv-rm-files is on, so a
	// deleted path also shows up in `fossil extras`. Keep the tracked status.
	seen := make(map[string]bool, len(changes))
	for _, c := range changes {
		seen[c.Path] = true
	}
	for _, u := range untracked {
		if !seen[u.Path] {
			changes = append(changes, u)
		}
	}
	return changes, nil
}

// ChangedFilesScoped returns changed files for a scope. Fossil has no staging
// area, so "staged" and "unstaged" return nil.
func (f *FossilVCS) ChangedFilesScoped(scope, baseRef string) ([]FileChange, error) {
	if scope == "branch" {
		return f.ChangedFilesFromBaseInDir(baseRef, "")
	}
	return nil, nil
}

// ChangedFilesForCommit returns the files changed in a single check-in.
func (f *FossilVCS) ChangedFilesForCommit(sha, dir string) ([]FileChange, error) {
	out, err := FossilCommandInDir(dir, "diff", "-i", "--brief", "--checkin", sha)
	if err != nil {
		return nil, err
	}
	return parseFossilChanges(out), nil
}

// FileDiffUnified returns parsed diff hunks for a file against a base ref.
func (f *FossilVCS) FileDiffUnified(path, baseRef, dir string, ignoreWhitespace bool) ([]DiffHunk, error) {
	return f.FileDiffUnifiedCtx(context.Background(), path, baseRef, dir, ignoreWhitespace)
}

// FileDiffUnifiedCtx is like FileDiffUnified but accepts a context for cancellation.
// An empty baseRef diffs the working checkout against its check-in.
func (f *FossilVCS) FileDiffUnifiedCtx(ctx context.Context, path, baseRef, dir string, ignoreWhitespace bool) ([]DiffHunk, error) {
	args := fossilDiffArgs(ignoreWhitespace)
	if baseRef != "" {
		args = append(args, "--from", fossilRef(baseRef))
	}
	args = append(args, path)
	return f.runDiff(ctx, dir, args)
}

// FileDiffScoped returns diff hunks for a file using a scope-appropriate diff.
// Fossil has no staging area, so "staged" and "unstaged" return nil.
func (f *FossilVCS) FileDiffScoped(path, scope, baseRef, dir string, ignoreWhitespace bool) ([]DiffHunk, error) {
	if scope == "branch" {
		return f.FileDiffUnified(path, baseRef, dir, ignoreWhitespace)
	}
	return nil, nil
}

// FileDiffForCommit returns diff hunks for a file in a single check-in.
func (f *FossilVCS) FileDiffForCommit(path, sha, dir string, ignoreWhitespace bool) ([]DiffHunk, error) {
	args := append(fossilDiffArgs(ignoreWhitespace), "--checkin", sha, path)
	return f.runDiff(context.Background(), dir, args)
}

// FileDiffUnifiedNewFile returns diff hunks showing an entire file as added.
func (f *FossilVCS) FileDiffUnifiedNewFile(path string) ([]DiffHunk, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return FileDiffUnifiedNewFile(string(data)), nil
}

// FileDiffBetweenSHAs returns parsed diff hunks for path in the range baseSHA..headSHA.
func (f *FossilVCS) FileDiffBetweenSHAs(path, _ string, baseSHA, headSHA, dir string, ignoreWhitespace bool) ([]DiffHunk, error) {
	args := append(fossilDiffArgs(ignoreWhitespace), "--from", fossilRef(baseSHA), "--to", fossilRef(headSHA), path)
	return f.runDiff(context.Background(), dir, args)
}

// runDiff runs `fossil diff` and parses the unified output. Fossil exits
// non-zero for unresolvable names but zero whether or not there is a diff;
// output that contains hunks is parsed even when the exit status is non-zero.
func (f *FossilVCS) runDiff(ctx context.Context, dir string, args []string) ([]DiffHunk, error) {
	cmd := exec.CommandContext(ctx, "fossil", args...)
	cmd.Dir = fossilDir(dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if len(out) > 0 {
			return ParseUnifiedDiff(string(out)), nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("fossil %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return ParseUnifiedDiff(string(out)), nil
}

// fossilDiffArgs returns the leading arguments shared by every diff call:
// "-i" forces Fossil's internal diff engine (ignoring any configured
// external diff-command) and "-N" prints full hunks for added/deleted files.
func fossilDiffArgs(ignoreWhitespace bool) []string {
	args := []string{"diff", "-i", "-N"}
	if ignoreWhitespace {
		args = append(args, "-w")
	}
	return args
}

// CommitLog returns the check-ins reachable from headRef but not from baseRef,
// newest first. Fossil has no A..B range syntax, so this lists ancestors of
// each side and subtracts. When headRef is empty the current checkout is used.
func (f *FossilVCS) CommitLog(baseRef, headRef, dir string) ([]CommitInfo, error) {
	if baseRef == "" {
		return nil, nil
	}
	head := fossilRef(headRef)
	if head == "" {
		head = "current"
	}
	headOut, err := FossilCommandInDir(dir, "timeline", "ancestors", head, "-n", "0", "-t", "ci", "-F", fossilTimelineFormat)
	if err != nil {
		return nil, err
	}
	baseOut, err := FossilCommandInDir(dir, "timeline", "ancestors", fossilRef(baseRef), "-n", "0", "-t", "ci", "-F", "%H")
	if err != nil {
		return nil, err
	}
	exclude := make(map[string]bool)
	for _, line := range SplitNonEmpty(baseOut) {
		if !isFossilTimelineTrailer(line) {
			exclude[strings.TrimSpace(line)] = true
		}
	}
	var commits []CommitInfo
	for _, c := range parseFossilTimeline(headOut) {
		if !exclude[c.SHA] {
			commits = append(commits, c)
		}
	}
	return commits, nil
}

// WorkingTreeFingerprint returns a string representing the current checkout state.
func (f *FossilVCS) WorkingTreeFingerprint() string {
	out, err := exec.Command("fossil", "changes", "--differ").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// UntrackedFiles returns files present in the checkout but not managed by Fossil.
func (f *FossilVCS) UntrackedFiles(dir string) ([]FileChange, error) {
	out, err := FossilCommandInDir(dir, "extras")
	if err != nil {
		return nil, err
	}
	var changes []FileChange
	for _, path := range SplitNonEmpty(out) {
		changes = append(changes, FileChange{Path: path, Status: "untracked"})
	}
	return changes, nil
}

// AllTrackedFiles returns all managed files plus untracked non-ignored files.
func (f *FossilVCS) AllTrackedFiles(dir string) ([]string, error) {
	trackedOut, err := FossilCommandInDir(dir, "ls")
	if err != nil {
		return nil, err
	}
	files := SplitNonEmpty(trackedOut)
	untracked, err := f.UntrackedFiles(dir)
	if err != nil {
		return files, nil //nolint:nilerr // graceful: return tracked files even if extras listing fails
	}
	for _, fc := range untracked {
		files = append(files, fc.Path)
	}
	return files, nil
}

// RemoteBranches returns nil: Fossil branches are repository-global and are
// exposed through the compare picker's local list instead.
func (f *FossilVCS) RemoteBranches(_ string) ([]string, error) { return nil, nil }

// Branches returns every branch name in the repository.
func (f *FossilVCS) Branches(dir string) ([]string, error) {
	out, err := FossilCommandInDir(dir, "branch", "list")
	if err != nil {
		return nil, err
	}
	return parseFossilBranchList(out), nil
}

// DiffNumstat returns per-file addition/deletion counts against baseRef.
func (f *FossilVCS) DiffNumstat(baseRef, dir string) (map[string]NumstatEntry, error) {
	if baseRef == "" {
		return nil, nil
	}
	out, err := FossilCommandInDir(dir, "diff", "-i", "--numstat", "--from", fossilRef(baseRef))
	if err != nil {
		return nil, err
	}
	return parseFossilNumstat(out), nil
}

// DiffNumstatBetweenSHAs returns per-file addition/deletion counts for baseSHA..headSHA.
func (f *FossilVCS) DiffNumstatBetweenSHAs(baseSHA, headSHA, dir string) (map[string]NumstatEntry, error) {
	if baseSHA == "" || headSHA == "" {
		return nil, fmt.Errorf("diff numstat between SHAs requires both base and head")
	}
	out, err := FossilCommandInDir(dir, "diff", "-i", "--numstat", "--from", fossilRef(baseSHA), "--to", fossilRef(headSHA))
	if err != nil {
		return nil, err
	}
	return parseFossilNumstat(out), nil
}

// UserName returns the default Fossil user for this checkout.
func (f *FossilVCS) UserName() string {
	out, err := exec.Command("fossil", "user", "default").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// FileContentAtRef returns the content of a file at the given check-in.
func (f *FossilVCS) FileContentAtRef(path, ref, dir string) (string, error) {
	if ref == "" {
		return "", nil
	}
	out, err := FossilCommandInDir(dir, "cat", "-r", fossilRef(ref), path)
	if err != nil {
		return "", err
	}
	return out, nil
}

// FileStatusInRepo returns the status of a single file relative to baseRef.
func (f *FossilVCS) FileStatusInRepo(path, baseRef, dir string) string {
	if baseRef == "" {
		return "modified"
	}
	out, err := FossilCommandInDir(dir, "diff", "-i", "--brief", "--from", fossilRef(baseRef), path)
	if err != nil {
		return ""
	}
	if changes := parseFossilChanges(out); len(changes) > 0 {
		return changes[0].Status
	}
	if extras, err := FossilCommandInDir(dir, "extras", path); err == nil && len(SplitNonEmpty(extras)) > 0 {
		return "untracked"
	}
	return ""
}

// ChangedFilesBetweenSHAs returns the files changed in the range baseSHA..headSHA.
func (f *FossilVCS) ChangedFilesBetweenSHAs(baseSHA, headSHA, dir string) ([]FileChange, error) {
	out, err := FossilCommandInDir(dir, "diff", "-i", "--brief", "--from", fossilRef(baseSHA), "--to", fossilRef(headSHA))
	if err != nil {
		return nil, err
	}
	return parseFossilChanges(out), nil
}

// ReadFileAtSHA returns the bytes of path at the given check-in, or (nil, nil)
// when the path does not exist there. An unresolvable check-in is an error.
func (f *FossilVCS) ReadFileAtSHA(sha, path, dir string) ([]byte, error) {
	cmd := exec.Command("fossil", "cat", "-r", fossilRef(sha), path)
	cmd.Dir = fossilDir(dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := stderr.String()
		if strings.Contains(msg, "does not exist in check-in") {
			return nil, nil
		}
		return nil, fmt.Errorf("fossil cat -r %s %s: %w: %s", sha, path, err, strings.TrimSpace(msg))
	}
	return out, nil
}

// HasObject reports whether sha resolves to an artifact in the local repository.
func (f *FossilVCS) HasObject(sha, dir string) bool {
	_, err := FossilResolve(dir, sha)
	return err == nil
}

// HasStagingArea returns false because Fossil has no staging area.
func (f *FossilVCS) HasStagingArea() bool { return false }

// SkipDirNames returns directory names to skip during walks. Fossil's
// checkout marker is a file, so there is no metadata directory to skip.
func (f *FossilVCS) SkipDirNames() []string { return nil }

// detectFossilDefaultBranch returns "trunk" when it resolves in the current
// checkout, or "" when the repository has no trunk (renamed or empty).
func detectFossilDefaultBranch() string {
	if _, err := FossilResolve("", "trunk"); err == nil {
		return "trunk"
	}
	return ""
}

// FossilResolve resolves a check-in name (branch, tag, abbreviated or full
// hash, "current", "trunk", …) to its full artifact hash.
func FossilResolve(dir, name string) (string, error) {
	out, err := FossilCommandInDir(dir, "whatis", fossilRef(name))
	if err != nil {
		return "", err
	}
	for _, line := range SplitNonEmpty(out) {
		if rest, ok := strings.CutPrefix(line, "artifact:"); ok {
			if hash := strings.TrimSpace(rest); hash != "" {
				return hash, nil
			}
		}
	}
	return "", fmt.Errorf("fossil whatis %s: unknown name", name)
}

// FossilAncestors lists up to maxDepth check-in hashes reachable from ref,
// newest first. maxDepth <= 0 means unlimited.
func FossilAncestors(dir, ref string, maxDepth int) ([]string, error) {
	out, err := FossilCommandInDir(dir, "timeline", "ancestors", fossilRef(ref), "-n", strconv.Itoa(max(maxDepth, 0)), "-t", "ci", "-F", "%H")
	if err != nil {
		return nil, err
	}
	var shas []string
	for _, line := range SplitNonEmpty(out) {
		if !isFossilTimelineTrailer(line) {
			shas = append(shas, strings.TrimSpace(line))
		}
	}
	return shas, nil
}

// fossilTopicChain returns check-ins reachable from the current checkout but
// not from defaultBranch, newest first (the "topic branch" stack). maxDepth
// caps how far back the checkout's ancestry is scanned; <= 0 is unlimited.
// When defaultBranch is empty or unresolvable, every scanned ancestor is returned.
func fossilTopicChain(dir, defaultBranch string, maxDepth int) []string {
	head, err := FossilAncestors(dir, "current", maxDepth)
	if err != nil {
		return nil
	}
	exclude := make(map[string]bool)
	if defaultBranch != "" {
		if base, err := FossilAncestors(dir, defaultBranch, 0); err == nil {
			for _, sha := range base {
				exclude[sha] = true
			}
		}
	}
	var out []string
	for _, sha := range head {
		if !exclude[sha] {
			out = append(out, sha)
		}
	}
	return out
}

// FossilCommitSubject returns the first line of a check-in's comment.
func FossilCommitSubject(dir, sha string) string {
	out, err := FossilCommandInDir(dir, "timeline", "ancestors", sha, "-n", "1", "-t", "ci", "-F", "%c")
	if err != nil {
		return ""
	}
	for _, line := range SplitNonEmpty(out) {
		if !isFossilTimelineTrailer(line) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// FossilCommandInDir runs a fossil subcommand in the given directory (or the
// checkout root when dir is empty) and returns stdout. Fossil prints paths
// relative to cwd, so running at the root keeps them repo-relative.
func FossilCommandInDir(dir string, args ...string) (string, error) {
	cmd := exec.Command("fossil", args...)
	cmd.Dir = fossilDir(dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("fossil %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// fossilDir returns dir, or the checkout root containing cwd when dir is
// empty. Returns "" (inherit cwd) if no checkout root can be found.
func fossilDir(dir string) string {
	if dir != "" {
		return dir
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if root, ok := fossilCheckoutRootFrom(cwd); ok {
		return root
	}
	return ""
}

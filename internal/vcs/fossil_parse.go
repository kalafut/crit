package vcs

import (
	"strconv"
	"strings"
)

// fossilStatusMap maps the status words printed by `fossil changes` and
// `fossil diff --brief` to crit status strings. Words not listed here
// (e.g. "MOVED_FILE", "NOT_A_FILE") are skipped.
var fossilStatusMap = map[string]string{
	// fossil changes
	"EDITED":           "modified",
	"UPDATED_BY_MERGE": "modified",
	"ADDED_BY_MERGE":   "added",
	"CONFLICT":         "modified",
	"EXECUTABLE":       "modified",
	"UNEXEC":           "modified",
	"SYMLINK":          "modified",
	"UNLINK":           "modified",
	"RENAMED":          "renamed",
	"MISSING":          "deleted",
	"EXTRA":            "untracked",
	// shared / fossil diff --brief
	"ADDED":   "added",
	"DELETED": "deleted",
	"CHANGED": "modified",
}

// parseFossilChanges parses the output of `fossil changes [--differ]` or
// `fossil diff --brief`. Each line is `<STATUS>  <path>`; renames appear
// as `<STATUS>  <old>  ->  <new>`.
func parseFossilChanges(output string) []FileChange {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return nil
	}
	var changes []FileChange
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		word, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		status, ok := fossilStatusMap[word]
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		if rest == "" {
			continue
		}
		if oldPath, newPath, isRename := strings.Cut(rest, "->"); isRename && strings.HasSuffix(oldPath, " ") && strings.HasPrefix(newPath, " ") {
			changes = append(changes, FileChange{
				Path:    strings.TrimSpace(newPath),
				OldPath: strings.TrimSpace(oldPath),
				Status:  "renamed",
			})
			continue
		}
		changes = append(changes, FileChange{Path: rest, Status: status})
	}
	return changes
}

// parseFossilNumstat parses `fossil diff --numstat` output:
//
//	INSERTED    DELETED
//	       3          1 f.txt
//	       1          0 new.txt
//	       4          1 TOTAL over 2 changed files
//
// The header and TOTAL rows are skipped.
func parseFossilNumstat(output string) map[string]NumstatEntry {
	result := make(map[string]NumstatEntry)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		adds, err1 := strconv.Atoi(fields[0])
		dels, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue // header row
		}
		if fields[2] == "TOTAL" {
			continue
		}
		// Rejoin the remainder so paths with spaces survive.
		idx := strings.Index(line, fields[1])
		path := strings.TrimSpace(line[idx+len(fields[1]):])
		result[path] = NumstatEntry{Additions: adds, Deletions: dels}
	}
	return result
}

// fossilTimelineFormat is the -F template used by CommitLog. %c collapses
// newlines to spaces, so every check-in is exactly fossilTimelineFields lines
// and no record separator is needed.
const fossilTimelineFormat = "%H%n%h%n%a%n%d%n%c"

const fossilTimelineFields = 5

// parseFossilTimeline parses `fossil timeline -t ci -F fossilTimelineFormat`
// output into CommitInfo values, newest first. Trailer lines such as
// "+++ end of timeline (4) +++" and "--- entry limit (1) reached ---" are dropped.
func parseFossilTimeline(output string) []CommitInfo {
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if isFossilTimelineTrailer(line) {
			continue
		}
		lines = append(lines, line)
	}
	var commits []CommitInfo
	for i := 0; i+fossilTimelineFields <= len(lines); i += fossilTimelineFields {
		sha := strings.TrimSpace(lines[i])
		if sha == "" {
			continue
		}
		commits = append(commits, CommitInfo{
			SHA:      sha,
			ShortSHA: strings.TrimSpace(lines[i+1]),
			Author:   strings.TrimSpace(lines[i+2]),
			Date:     strings.TrimSpace(lines[i+3]),
			Message:  strings.TrimSpace(lines[i+4]),
		})
	}
	return commits
}

func isFossilTimelineTrailer(line string) bool {
	return strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "--- ")
}

// parseFossilMergeBase extracts the hash from `fossil merge-base` output
// ("pivot=<hash>").
func parseFossilMergeBase(output string) string {
	out := strings.TrimSpace(output)
	if _, hash, ok := strings.Cut(out, "="); ok {
		return strings.TrimSpace(hash)
	}
	return out
}

// parseFossilBranchList parses `fossil branch list` output. The current
// branch is marked with a leading "*".
func parseFossilBranchList(output string) []string {
	var names []string
	for _, line := range SplitNonEmpty(output) {
		name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*"))
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

package postgresstorage

import (
	"sort"
	"strings"
)

// Keys are slash-separated paths, as certmagic writes them ("certificates/acme.../example.com/...").
// The table holds only terminal keys (files). A "folder" is implied: it exists while at least one
// key lives under it, exactly as a directory on disk exists while it has files in it.

// folderPrefix turns a key into the prefix its descendants start with: "a/b" and "a/b/" both become
// "a/b/", and the root ("" or "/") becomes "", which every key starts with.
//
// The prefix is matched with starts_with(), a literal comparison, never with LIKE: in a LIKE pattern
// '_' and '%' are wildcards, and certmagic keys contain '_' ("wildcard_.example.com"), so an
// unescaped LIKE on "wildcard_.example.com/" would also match "wildcardX.example.com/".
func folderPrefix(key string) string {
	trimmed := strings.Trim(key, "/")
	if trimmed == "" {
		return ""
	}
	return trimmed + "/"
}

// hasFolderPrefix reports whether key lies under prefix (as built by folderPrefix). It is the Go
// twin of the SQL starts_with(key, prefix) and exists so the logic has a unit test.
func hasFolderPrefix(key, prefix string) bool {
	return strings.HasPrefix(key, prefix)
}

// listChildren turns the descendants of prefix (the keys stored under it) into what List returns.
//
// Non-recursive: the direct children only, files and folders alike, as FileStorage lists one
// directory level. Recursive: every descendant, including the implied folders on the way down, as
// FileStorage's filepath.Walk does. The result is sorted and has no duplicates. A descendant that does
// not actually start with prefix is ignored.
func listChildren(prefix string, descendants []string, recursive bool) []string {
	seen := make(map[string]struct{}, len(descendants))
	for _, key := range descendants {
		if !hasFolderPrefix(key, prefix) {
			continue
		}
		rest := strings.TrimPrefix(key, prefix)
		if rest == "" {
			continue
		}
		segments := strings.Split(rest, "/")
		if !recursive {
			seen[prefix+segments[0]] = struct{}{}
			continue
		}
		for i := range segments {
			seen[prefix+strings.Join(segments[:i+1], "/")] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

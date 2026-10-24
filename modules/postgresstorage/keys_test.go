package postgresstorage

import (
	"reflect"
	"testing"
)

func TestFolderPrefixNormalisesSlashesAndRoot(t *testing.T) {
	cases := map[string]string{
		"":         "",
		"/":        "",
		"a":        "a/",
		"a/":       "a/",
		"/a/b/":    "a/b/",
		"a/b":      "a/b/",
		"wild_%.x": "wild_%.x/",
	}
	for in, want := range cases {
		if got := folderPrefix(in); got != want {
			t.Errorf("folderPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// '_' and '%' are LIKE wildcards; a prefix holding them must match only itself, literally.
func TestPrefixWithLikeWildcardsMatchesOnlyItself(t *testing.T) {
	prefix := folderPrefix("certificates/wildcard_.example.com")
	for key, want := range map[string]bool{
		"certificates/wildcard_.example.com/wildcard_.example.com.crt": true,
		"certificates/wildcardX.example.com/wildcardX.example.com.crt": false,
		"certificates/wildcard_Xexample.com/x.crt":                     false,
		"certificates/wildcard_.example.comX/x.crt":                    false,
		"certificates/wildcard_.example.com":                           false, // the folder name itself is not under it
	} {
		if got := hasFolderPrefix(key, prefix); got != want {
			t.Errorf("hasFolderPrefix(%q, %q) = %v, want %v", key, prefix, got, want)
		}
	}
	percent := folderPrefix("a%b")
	if hasFolderPrefix("aXXb/c", percent) || hasFolderPrefix("ab/c", percent) {
		t.Errorf("prefix %q matched a key it does not literally prefix", percent)
	}
}

func TestListChildrenNonRecursiveGivesDirectChildrenWithFolders(t *testing.T) {
	keys := []string{
		"certificates/ca/a.example.com/a.example.com.crt",
		"certificates/ca/a.example.com/a.example.com.key",
		"certificates/ca/b.example.com/b.example.com.crt",
		"certificates/ca/top.json",
		"certificates/other/x",
		"elsewhere/y",
	}
	got := listChildren(folderPrefix("certificates/ca"), keys, false)
	want := []string{
		"certificates/ca/a.example.com",
		"certificates/ca/b.example.com",
		"certificates/ca/top.json",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("non-recursive = %v, want %v", got, want)
	}
}

func TestListChildrenRecursiveGivesEveryDescendantAndImpliedFolder(t *testing.T) {
	keys := []string{"a/b/c/d", "a/b/e", "a/f", "z/q"}
	got := listChildren(folderPrefix("a"), keys, true)
	want := []string{"a/b", "a/b/c", "a/b/c/d", "a/b/e", "a/f"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recursive = %v, want %v", got, want)
	}
	root := listChildren("", keys, false)
	if !reflect.DeepEqual(root, []string{"a", "z"}) {
		t.Errorf("root non-recursive = %v, want [a z]", root)
	}
}

func TestListChildrenIgnoresKeysOutsideThePrefix(t *testing.T) {
	got := listChildren(folderPrefix("wildcard_.example.com"), []string{"wildcardX.example.com/k"}, true)
	if len(got) != 0 {
		t.Errorf("got %v, want nothing", got)
	}
}

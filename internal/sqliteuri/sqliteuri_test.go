package sqliteuri

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSqliteURI(t *testing.T) {
	for in, want := range map[string]string{
		"/a b/r?#%.db":    "file:///a%20b/r%3F%23%25.db?mode=ro",
		`C:\Users\x\r.db`: "file:///C:/Users/x/r.db?mode=ro",
	} {
		in := in
		if !strings.HasPrefix(in, "/") && filepath.Separator == '/' {
			// ToSlash is a no-op off windows; feed the slashed form
			in = strings.ReplaceAll(in, `\`, "/")
		}
		if got := For(in, "mode=ro"); got != want {
			t.Errorf("For(%q) = %q, want %q", in, got, want)
		}
	}
}

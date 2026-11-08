// Package sqliteuri makes the file: URIs modernc.org/sqlite opens.
package sqliteuri

import (
	"fmt"
	"path/filepath"
	"strings"
)

// For is a file: uri for path, which has to survive a windows drive
// letter and backslashes as well as a '?' or '#' in a name.
func For(path string, query string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // C:/x -> /C:/x, which sqlite's windows vfs expects
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '%' || c == '?' || c == '#' || c == ' ' || c < 0x20 {
			fmt.Fprintf(&b, "%%%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	return "file://" + b.String() + "?" + query
}

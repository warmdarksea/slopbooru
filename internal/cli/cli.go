// Package cli is what every slopbooru command shares: the name it goes by
// and how it reports.
package cli

import (
	"fmt"
	"os"
)

// set at link time: -X slopbooru/internal/cli.Brand=llm-booru inside the
// llm-qwen project
var (
	Brand   = "slopbooru"
	Version = "dev"
)

// Logf writes one line to stderr, after the brand.
func Logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, Brand+": "+format+"\n", a...)
}

// Die logs and exits 1.
func Die(format string, a ...any) {
	Logf(format, a...)
	os.Exit(1)
}

// imgrec: the recognition pipeline. See DESIGN.md.
//
//	imgrec import-py-cache --py-db OLD.db --images DIR --manifest models.json --db NEW.db
//
// So far: rebuilding a store from the python pipeline's answer cache with
// the Go parsers -- the replay parity check, and the migration path.
package main

import (
	"fmt"
	"os"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: imgrec COMMAND [flags]

commands:
  import-py-cache   build a store from the python pipeline's answer cache`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "import-py-cache":
		err = importPyCache(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "imgrec:", err)
		os.Exit(1)
	}
}

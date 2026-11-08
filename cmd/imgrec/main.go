// imgrec: the recognition pipeline. See DESIGN.md.
//
//	imgrec run --specs DIR --manifest models.json [--db results.db] DIR
//	imgrec import-py-cache ...   (the parity test's tool)
package main

import (
	"fmt"
	"os"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: imgrec COMMAND [flags]

commands:
  run DIR           every picture in DIR on every model, until done
  import-py-cache   (parity test) a store from the python pipeline's answer cache`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
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

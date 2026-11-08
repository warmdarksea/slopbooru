// imgrec: the recognition pipeline. See DESIGN.md.
//
//	imgrec run --specs DIR --manifest models.json [--db results.db] DIR
//	imgrec serve [--listen 127.0.0.1:8100] ...
//	imgrec client add NAME [--admin]
//	imgrec submit --token T [--server URL] [--cacert F] [--models a,b] FILE...
//	imgrec cert --hosts NAME,... [--cert F --key F]
//	imgrec import-py-cache ...   (the parity test's tool)
package main

import (
	"fmt"
	"os"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: imgrec COMMAND [flags]

commands:
  run DIR           every picture in DIR on every model, until done (batch mode)
  serve             the HTTP API, with the scheduler behind it
  client            add NAME [--admin] | list | remove NAME: who may use the API
  submit FILE...    the API's client: upload, ask, wait, print the answers
  cert              a self-signed certificate for serve --tls-cert/--tls-key
  rebuild-store     rebuild the store (what the booru reads) from the records
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
	case "serve":
		err = serveCmd(os.Args[2:])
	case "client":
		err = clientCmd(os.Args[2:])
	case "submit":
		err = submitCmd(os.Args[2:])
	case "cert":
		err = certCmd(os.Args[2:])
	case "rebuild-store":
		err = rebuildCmd(os.Args[2:])
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

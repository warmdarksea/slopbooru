// slopbooru: a booru over an image recognition db, and the command that
// fills it.
//
//	slopbooru [serve] --db results.db --images DIR   browse it (internal/booru)
//	slopbooru classify --images DIR                  ask models about pictures (internal/classify)
package main

import (
	"os"

	"slopbooru/internal/booru"
	"slopbooru/internal/classify"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "classify":
			classify.Main(args[1:])
			return
		case "serve":
			args = args[1:]
		}
	}
	booru.Main(args)
}

// Command footops-mac is FootOps on the Mac (panel-tasarim §29): the servers' state from
// R2, the sites visited from the Mac; FootOps.app runs it for what it shows.
package main

import (
	"os"

	"github.com/swbilisim/FootOps/internal/mac"
)

func main() {
	os.Exit(mac.Run(os.Args[1:], os.Stdout, os.Stderr))
}

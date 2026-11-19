// Command loka shares files and directories with computers on the same local
// network.
package main

import (
	"flag"
	"fmt"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("loka", version)
	}
}

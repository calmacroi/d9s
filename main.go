// Command d9s is a k9s-style terminal UI for the local Docker and Docker Compose environment.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

var version = "dev"

func main() {
	ctxName := flag.String("context", "", "docker context to use (default: current context or DOCKER_HOST)")
	refresh := flag.Duration("refresh", 2*time.Second, "table refresh interval")
	view := flag.String("view", "containers", "initial view, e.g. containers, compose, \"svc symbiocommerce\", images")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "d9s %s — k9s-style TUI for Docker / Docker Compose\n\nUsage: d9s [flags]\n\n", version)
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("d9s", version)
		return
	}
	if *refresh < 500*time.Millisecond {
		*refresh = 500 * time.Millisecond
	}

	d, err := NewDocker(*ctxName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "d9s: cannot create docker client:", err)
		os.Exit(1)
	}
	defer d.Close()

	if err := NewApp(d, *refresh, *view).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "d9s:", err)
		os.Exit(1)
	}
}

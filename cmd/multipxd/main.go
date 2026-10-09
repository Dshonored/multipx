// Command multipxd is the multipx daemon: it owns PTY sessions and serves
// them to multipx clients over QUIC.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"

	"multipx/internal/daemon"
)

func main() {
	addr := flag.String("addr", daemon.DefaultAddr, "address to listen on")
	printToken := flag.Bool("print-token", false, "print this machine's auth token and exit — "+
		"run this over SSH on a remote machine, then paste the result into the app when adding that host")
	flag.Parse()

	if *printToken {
		tok, err := daemon.EnsureToken()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(tok)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	d := daemon.New(*addr)
	if err := d.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

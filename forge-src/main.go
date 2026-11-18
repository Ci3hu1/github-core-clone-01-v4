package main

// forge: a static, stdlib-only GitHub-compatible git forge.
// Listens on an ephemeral port (or --port), prints "PORT <n>" on stdout,
// and serves until killed. The launcher (main.ts) owns the boot protocol.

import (
	"flag"
	"fmt"
	"net"
	"os"
)

func main() {
	port := flag.Int("port", 0, "port to listen on (0 = ephemeral)")
	flag.Parse()

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "forge: listen:", err)
		os.Exit(1)
	}
	addr := ln.Addr().(*net.TCPAddr)
	fmt.Printf("PORT %d\n", addr.Port)
	// Make sure the port line reaches the launcher before serving.
	os.Stdout.Sync()

	srv := &Server{store: newStore()}
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go srv.serveConn(conn)
	}
}

// Package main is a simple HTTP-over-TCP echo backend used for local testing.
// Start it with: go run ./cmd/backend :9000
package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: backend <:port>")
		os.Exit(1)
	}
	port := os.Args[1]

	listener, err := net.Listen("tcp", port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen error:", err)
		os.Exit(1)
	}
	defer listener.Close()
	fmt.Printf("backend listening on %s\n", listener.Addr())

	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("accept error:", err)
			continue
		}
		go handle(conn, port)
	}
}

func handle(conn net.Conn, port string) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	request, err := reader.ReadString('\n')
	if err != nil {
		return
	}

	parts := strings.Split(request, " ")
	if len(parts) < 3 {
		return
	}

	path := parts[1]
	fmt.Printf("[%s] %s %s\n", port, strings.TrimSpace(parts[0]), path)

	body := "Hello from " + port
	switch path {
	case "/hello":
		body = "Hey! How are you?"
	case "/home":
		body = "This is the home page"
	}

	fmt.Fprintf(conn,
		"HTTP/1.1 200 OK\r\n"+
			"Content-Type: text/plain\r\n"+
			"Access-Control-Allow-Origin: *\r\n"+
			"Content-Length: %d\r\n\r\n%s",
		len(body), body,
	)
}

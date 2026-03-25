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
		fmt.Println("Usage: backend <:port>")
		os.Exit(1)
	}
	port := os.Args[1]

	listener, err := net.Listen("tcp", port)
	if err != nil {
		fmt.Println("Error:", err)
		os.Exit(1)
	}
	defer listener.Close()
	fmt.Printf("Backend listening on %s\n", listener.Addr())

	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("Accept error:", err)
			continue
		}
		go handleConnection(conn, port)
	}
}

func handleConnection(conn net.Conn, port string) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	request, err := reader.ReadString('\n')
	if err != nil {
		fmt.Println("Read error:", err)
		return
	}

	parts := strings.Split(request, " ")
	if len(parts) < 3 {
		fmt.Println("Invalid HTTP request")
		return
	}

	path := parts[1]
	fmt.Printf("[%s] %s %s\n", port, parts[0], path)

	body := "Hello from " + port
	if path == "/hello" {
		body = "Hey! How are you?"
	} else if path == "/home" {
		body = "This is the home page"
	}

	response := fmt.Sprintf(
		"HTTP/1.1 200 OK\r\n"+
			"Content-Type: text/plain\r\n"+
			"Access-Control-Allow-Origin: *\r\n"+
			"Content-Length: %d\r\n"+
			"\r\n%s",
		len(body), body,
	)

	conn.Write([]byte(response))
}
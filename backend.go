package main

import "net"
import "fmt"
import "bufio"
import "strings"
import "os"

func main() {
	port := os.Args[1]
	listener, err := net.Listen("tcp", port)
	
	if err!=nil {
		fmt.Println("Error",err)
	}
	defer listener.Close()
	fmt.Printf("Listening on %s\n", listener.Addr())
	
	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("Accept error:", err)
			continue
		}
		go handleConnection(conn)
	}
}

func handleConnection(conn net.Conn) {
	defer conn.Close()
	
	reader := bufio.NewReader(conn)

	request, err := reader.ReadString('\n')

	if err != nil {
		fmt.Println("Error", err)
	}

	reqArr := strings.Split(request, " ")

	if len(reqArr) < 3 {
	fmt.Println("Invalid HTTP request")
	return
	}

	path := reqArr[1]
	fmt.Println("Method:", reqArr[0])
	fmt.Println("Path:", reqArr[1])
	fmt.Println("Version:", reqArr[2])

	body:= "Hello from 9000"
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
			"\r\n"+
			"%s",
		len(body),
		body,
	)

	conn.Write([]byte(response))

}
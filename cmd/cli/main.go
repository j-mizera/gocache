package main

import (
	"bufio"
	"errors"
	"fmt"
	"gocache/pkg/resp"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
)

const (
	defaultHost = "localhost"
	defaultPort = 6379
)

func main() {
	conn, err := net.Dial("tcp", net.JoinHostPort(defaultHost, strconv.Itoa(defaultPort)))
	if err != nil {
		fmt.Printf("Failed to connect to server: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	reader := bufio.NewReader(os.Stdin)
	respReader := resp.NewReader(conn)
	respWriter := resp.NewWriter(conn)

	fmt.Println("Connected to gocache server. Type 'QUIT' to exit.")
	for {
		fmt.Print("> ")
		input, err := reader.ReadString('\n')
		if err != nil {
			fmt.Printf("Error reading input: %v\n", err)
			break
		}

		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}

		if strings.ToUpper(input) == "QUIT" {
			respWriter.Write(resp.Value{
				Type:  resp.Array,
				Array: []resp.Value{{Type: resp.BulkString, Str: "QUIT"}},
			})
			break
		}

		parts := strings.Fields(input)
		respParts := make([]resp.Value, len(parts))
		for i, p := range parts {
			respParts[i] = resp.MarshalBulkString(p)
		}

		respWriter.Write(resp.Value{
			Type:  resp.Array,
			Array: respParts,
		})

		response, err := respReader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Println("Server closed connection")
			} else {
				fmt.Printf("Error reading response: %v\n", err)
			}
			break
		}
		printResponse(response)
	}
}

func printResponse(v resp.Value) {
	switch v.Type {
	case resp.SimpleString:
		fmt.Println(v.Str)
	case resp.Error:
		fmt.Println(v.Str)
	case resp.Integer:
		fmt.Println(v.Integer)
	case resp.BulkString:
		fmt.Println(v.Str)
	case resp.Array:
		for _, item := range v.Array {
			printResponse(item)
		}
	}
}

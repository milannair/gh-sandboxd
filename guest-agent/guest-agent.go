package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
)

type Payload struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

func main() {
	socket := "/run/agent/agent.sock"

	out, _ := os.ReadFile("/tmp/stdout")
	errb, _ := os.ReadFile("/tmp/stderr")
	code := 0

	if b, err := os.ReadFile("/tmp/exitcode"); err == nil {
		fmt.Sscanf(string(b), "%d", &code)
	}

	conn, err := net.Dial("unix", socket)
	if err != nil {
		os.Exit(1)
	}
	defer conn.Close()

	_ = json.NewEncoder(conn).Encode(Payload{
		Stdout:   string(out),
		Stderr:   string(errb),
		ExitCode: code,
	})
}

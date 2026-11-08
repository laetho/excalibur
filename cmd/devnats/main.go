// Command devnats reuses a local JetStream server when one is already running,
// otherwise it starts the pinned development server used by the Taskfile.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
)

const serverVersion = "v2.14.9"

func main() {
	port := 42222
	value := os.Getenv("EXCALIBUR_DEV_NATS_PORT")
	if value == "" {
		value = os.Getenv("EXCALIWAILS_DEV_NATS_PORT") // Backward compatibility.
	}
	if value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 65535 {
			fmt.Fprintf(os.Stderr, "invalid EXCALIBUR_DEV_NATS_PORT %q\n", value)
			os.Exit(2)
		}
		port = parsed
	}
	serverURL := fmt.Sprintf("nats://127.0.0.1:%d", port)
	connection, err := nats.Connect(serverURL, nats.Timeout(350*time.Millisecond), nats.Name("Excalibur dev check"))
	if err == nil {
		defer connection.Close()
		js, jsErr := connection.JetStream()
		if jsErr == nil {
			_, jsErr = js.AccountInfo()
		}
		if jsErr != nil {
			fmt.Fprintf(os.Stderr, "NATS is already running at %s but JetStream is unavailable: %v\n", serverURL, jsErr)
			os.Exit(1)
		}
		fmt.Printf("Reusing JetStream-enabled NATS server at %s\n", serverURL)
		return
	}

	command := exec.Command("go", "run", "github.com/nats-io/nats-server/v2@"+serverVersion,
		"--jetstream", "--store_dir", ".dev/nats", "--addr", "127.0.0.1", "--port", strconv.Itoa(port))
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err = command.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "local NATS server stopped: %v\n", err)
		os.Exit(1)
	}
}

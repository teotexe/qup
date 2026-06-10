package main

import (
	"fmt"
	"os"
	"strings"

	"qup/cmd/connect"
	"qup/cmd/share"
)

func main() {
	if len(os.Args) < 2 {
		printGeneralUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	// Normalize command (strip leading - or --)
	cmdClean := strings.TrimPrefix(cmd, "-")
	cmdClean = strings.TrimPrefix(cmdClean, "-")

	switch cmdClean {
	case "share":
		share.Run(os.Args[2:])
	case "connect":
		connect.Run(os.Args[2:])
	case "help", "h":
		printGeneralUsage()
	default:
		fmt.Printf("Unknown command: %s\n\n", cmd)
		printGeneralUsage()
		os.Exit(1)
	}
}

func printGeneralUsage() {
	fmt.Println("Usage: ./qup <command> [flags] [args]")
	fmt.Println("\nAvailable commands:")
	fmt.Println("  share    Start the AuraShare Host (Bob) to share screen/audio")
	fmt.Println("  connect  Start the AuraShare Receiver (Alice) to connect and view a stream")
	fmt.Println("\nYou can also use legacy flags:")
	fmt.Println("  ./qup -share [flags]")
	fmt.Println("  ./qup -connect [flags]")
	fmt.Println("\nRun './qup <command> -help' for more information on a command's flags.")
}

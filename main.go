package main

import (
	"os"

	"github.com/supersuit-tech/string-can/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}

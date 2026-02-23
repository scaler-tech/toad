package main

import (
	"os"

	"github.com/hergen/toad/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}

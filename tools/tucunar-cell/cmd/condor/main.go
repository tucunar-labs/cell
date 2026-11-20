package main

import (
	"tucunar-cell"
	"tucunar-cell/cmd/internal/command"
)

func main() { command.Run(canarynode.RunCondor) }

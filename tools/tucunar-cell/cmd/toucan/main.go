package main

import (
	"context"

	"tucunar-cell"
	"tucunar-cell/cmd/internal/command"
)

func main() {
	command.Run(func(ctx context.Context) error { return canarynode.RunCapability(ctx, "toucan") })
}

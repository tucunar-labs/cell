package command

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func Run(fn func(context.Context) error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := fn(ctx); err != nil {
		log.Fatal(err)
	}
}

package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/aimount/aimount-cli/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	env := map[string]string{}
	for _, v := range os.Environ() {
		k, value, ok := strings.Cut(v, "=")
		if ok {
			env[k] = value
		}
	}
	exit := cli.Main(ctx, os.Args[1:], env, os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(exit)
}

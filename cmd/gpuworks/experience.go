package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/experience"
)

// experienceCommand runs the experience environment (internal/experience).
// The server's settings page starts it (internal/server/experiencelaunch);
// it is not meant to be run by hand, so usage does not list it.
func experienceCommand(args []string) error {
	var cfg experience.Config
	fs := flag.NewFlagSet("experience", flag.ContinueOnError)
	listen := fs.String("listen", "", "comma-separated console addresses")
	fs.StringVar(&cfg.Addr, "addr", "", "one address for everything, instead of --listen")
	fs.StringVar(&cfg.TLSCertFile, "tls-cert", "", "TLS certificate for the public listeners")
	fs.StringVar(&cfg.TLSKeyFile, "tls-key", "", "TLS key for the public listeners")
	fs.StringVar(&cfg.DataDir, "data", "", "data directory; nothing outside it is written")
	fs.StringVar(&cfg.PasswordFile, "password-file", "", "file holding the console password")
	parent := fs.Int("parent-pid", 0, "exit when this process (the server that started the environment) is gone")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for _, addr := range strings.Split(*listen, ",") {
		if addr = strings.TrimSpace(addr); addr != "" {
			cfg.Listen = append(cfg.Listen, addr)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *parent > 0 {
		go followParent(ctx, *parent, stop)
	}
	// The environment's own records stay terse: the console's access log
	// would otherwise grow with the live traffic it keeps going.
	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	slog.SetDefault(quiet)
	return experience.Run(ctx, cfg, quiet)
}

// followParent stops the environment once the server that started it is
// gone, so a restarted server never finds the port held by an orphan.
func followParent(ctx context.Context, pid int, stop func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		if os.Getppid() != pid {
			stop()
			return
		}
	}
}

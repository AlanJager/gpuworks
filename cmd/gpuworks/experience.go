package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"gpuworks.ai/gpuworks/internal/experience"
	"gpuworks.ai/gpuworks/internal/parentwatch"
	"gpuworks.ai/gpuworks/internal/server/experiencelaunch"
)

// experienceCommand runs the experience environment (internal/experience).
// The server's settings page starts it (internal/server/experiencelaunch);
// it is not meant to be run by hand, so usage does not list it.
func experienceCommand(args []string) error {
	var cfg experience.Config
	fs := flag.NewFlagSet(experiencelaunch.Command, flag.ContinueOnError)
	listen := fs.String(experiencelaunch.FlagListen, "", "comma-separated console addresses")
	fs.StringVar(&cfg.Addr, experiencelaunch.FlagAddr, "", "one address for everything, instead of --listen")
	fs.StringVar(&cfg.TLSCertFile, experiencelaunch.FlagTLSCert, "", "TLS certificate for the public listeners")
	fs.StringVar(&cfg.TLSKeyFile, experiencelaunch.FlagTLSKey, "", "TLS key for the public listeners")
	fs.StringVar(&cfg.DataDir, experiencelaunch.FlagData, "", "data directory; nothing outside it is written")
	fs.StringVar(&cfg.PasswordFile, experiencelaunch.FlagPasswordFile, "", "file holding the console password")
	parent := fs.Int(experiencelaunch.FlagParentPID, 0, "exit when this process (the server that started the environment) is gone")
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
		// A restarted server must never find the port held by an orphan.
		go parentwatch.Follow(ctx, *parent, stop)
	}
	// The environment's own records stay terse: the console's access log
	// would otherwise grow with the live traffic it keeps going.
	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	slog.SetDefault(quiet)
	return experience.Run(ctx, cfg, quiet)
}

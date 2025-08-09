package main

import (
	"flag"
	"github.com/konpure/Kon-Agent/internal/config"
	"github.com/konpure/Kon-Agent/internal/edge-agent"
	"log/slog"
	"os"
)

func main() {
	var cfgPath = flag.String("config", "config.yaml", "Path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}
	if err := edge_agent.New(cfg).Run(); err != nil {
		slog.Error("run edge_agent", "err", err)
		os.Exit(1)
	}
}

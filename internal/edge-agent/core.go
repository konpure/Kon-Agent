package edge_agent

import (
	"github.com/konpure/Kon-Agent/internal/config"
	"log/slog"
)

type Core struct {
	cfg *config.Config
}

func New(cfg *config.Config) *Core {
	return &Core{cfg: cfg}
}

func (c *Core) Run() error {
	slog.Info("Agent started", "config", c.cfg)
	return nil
}

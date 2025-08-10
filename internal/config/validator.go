package config

import (
	"fmt"
	"time"
)

func (c *Config) Validate() error {
	// Make sure Server address is not empty
	if c.Server == "" {
		return fmt.Errorf("server address is required")
	}

	// Check plugins config
	for name, plugin := range c.Plugins {
		if plugin.Enable {
			if plugin.Period <= 0 {
				return fmt.Errorf("plugin %s: period must be positive", name)
			}

			if plugin.Period < 100*time.Millisecond {
				return fmt.Errorf("plugin %s: period to short, minimum 100ms", name)
			}
		}
	}

	// Check cache config
	if c.Cache.Path == "" {
		c.Cache.Path = "/tmp/edge_agent.cache"
	}
	return nil
}

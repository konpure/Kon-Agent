package plugin

import "time"

type Event struct {
	Name   string
	Time   int64
	Labels map[string]string
	Values float64
}

type PluginFactory func(config PluginConfig) Plugin

type Plugin interface {
	Name() string
	Run(out chan<- Event) error
	Stop() error
	Config() PluginConfig
}

type PluginConfig struct {
	Enable     bool          `yaml:"enable"`
	Period     time.Duration `yaml:"period"`
	PluginType string        `yaml:"type"`
}

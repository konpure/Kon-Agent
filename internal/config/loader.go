package config

import (
	"github.com/konpure/Kon-Agent/pkg/plugin"
	"gopkg.in/yaml.v3"
	"os"
)

type Config struct {
	Server   string                         `yaml:"server"`
	ClientId string                         `yaml:"client_id"`
	Plugins  map[string]plugin.PluginConfig `yaml:"plugins"`
	Cache    struct {
		Path    string `yaml:"path"`
		MaxSize string `yaml:"max_size"`
	} `yaml:"cache"`
}

func Load(path string) (*Config, error) {
	f, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config Config
	if err := yaml.Unmarshal(f, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

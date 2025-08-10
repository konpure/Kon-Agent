package config

import (
	"gopkg.in/yaml.v3"
	"os"
	"time"
)

type Config struct {
	Server  string                `yaml:"server"`
	Plugins map[string]PluginConf `yaml:"plugins"`
	Cache   struct {
		Path    string `yaml:"path"`
		MaxSize string `yaml:"max_size"`
	} `yaml:"cache"`
}

type PluginConf struct {
	Enable bool          `yaml:"enable"`
	Period time.Duration `yaml:"period"`
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

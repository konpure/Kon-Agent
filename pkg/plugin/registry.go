package plugin

import "sync"

var (
	plugins = make(map[string]PluginFactory)
	mu      sync.RWMutex
)

func Register(name string, factory PluginFactory) {
	mu.Lock()
	defer mu.Unlock()
	plugins[name] = factory
}

func GetFactory(name string) (PluginFactory, bool) {
	mu.RLock()
	defer mu.RUnlock()
	factory, exists := plugins[name]
	return factory, exists
}

func GetRegisteredPlugins() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(plugins))
	for name := range plugins {
		names = append(names, name)
	}
	return names
}

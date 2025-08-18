package cpu

import (
	"bufio"
	"context"
	"github.com/konpure/Kon-Agent/pkg/plugin"
	"os"
	"strconv"
	"strings"
	"time"
)

type Collector struct {
	config plugin.PluginConfig
	stop   chan struct{}
}

func New(config plugin.PluginConfig) plugin.Plugin {
	return &Collector{
		config: config,
		stop:   make(chan struct{}),
	}
}

func init() {
	plugin.Register("cpu", New)
}

func (c *Collector) Name() string {
	return "cpu"
}

func (c *Collector) Config() plugin.PluginConfig {
	return c.config
}
func (c *Collector) Stop() error {
	close(c.stop)
	return nil
}
func (c *Collector) Run(ctx context.Context, out chan<- plugin.Event) error {
	tk := time.NewTicker(c.config.Period)
	defer tk.Stop()
	for {
		select {
		case <-tk.C:
			v, err := readCPU()
			if err != nil {
				continue
			}
			out <- plugin.Event{
				Name:   "cpu_usage_percent",
				Time:   time.Now().UnixNano(),
				Labels: map[string]string{"mode": "user"},
				Values: v,
			}
		case <-ctx.Done():
			return nil
		case <-c.stop:
			return nil
		}
	}
}

func readCPU() (float64, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	line, _ := bufio.NewReader(f).ReadString('\n')
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return 0, nil
	}
	var total, idle uint64
	for i := 1; i < len(fields); i++ {
		v, _ := strconv.ParseUint(fields[i], 10, 64)
		total += v
		if i == 4 {
			idle = v
		}
	}
	return float64(total-idle) / float64(total) * 100, nil
}

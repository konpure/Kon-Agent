package plugin

type Event struct {
	Name   string
	Time   int64
	Labels map[string]string
	Values float64
}

type Plugin interface {
	Name() string
	Run(out chan<- Event) error
	Stop() error
}

package pipeline

import "github.com/konpure/Kon-Agent/pkg/protocol"

// Item is a buffered metric together with its instrumentation scope
// (the producing plugin's name). It is the pipeline's internal data unit.
type Item struct {
	Scope  string
	Metric *protocol.Metric
}

package ebpfutil

import (
	"fmt"
	"log/slog"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
)

// Hook describes one kernel attachment point with its fallback chain.
// Strategies are tried in order: fentry (typed args via BTF), raw tracepoint
// (stable ABI), kprobe (function symbol, always available).
type Hook struct {
	FentryProg string // program with SEC("fentry/..."); empty skips fentry
	RawTPName  string // raw tracepoint name, e.g. "block_rq_issue"
	RawTPProg  string // program with SEC("raw_tracepoint/...")
	KprobeSym  string // kernel symbol, e.g. "blk_mq_start_request"
	KprobeProg string // program with SEC("kprobe/...")
}

// TryAttach attempts the hook's strategies in order and returns the first
// success, plus a description of which strategy won. Kernel compatibility is
// handled at runtime: a kernel without fentry/BTF or with a partially
// registered raw tracepoint table simply falls through to the next strategy.
func TryAttach(coll *ebpf.Collection, h Hook) (link.Link, string, error) {
	var lastErr error

	if h.FentryProg != "" {
		if p := coll.Programs[h.FentryProg]; p != nil {
			if l, err := link.AttachTracing(link.TracingOptions{Program: p}); err == nil {
				return l, "fentry", nil
			} else {
				lastErr = err
				slog.Debug("fentry attach failed, trying next strategy", "prog", h.FentryProg, "err", err)
			}
		}
	}

	if h.RawTPName != "" {
		if p := coll.Programs[h.RawTPProg]; p != nil {
			if l, err := link.AttachRawTracepoint(link.RawTracepointOptions{Name: h.RawTPName, Program: p}); err == nil {
				return l, "raw_tracepoint:" + h.RawTPName, nil
			} else {
				lastErr = err
				slog.Debug("raw tracepoint attach failed, trying next strategy", "name", h.RawTPName, "err", err)
			}
		}
	}

	if h.KprobeSym != "" {
		p := coll.Programs[h.KprobeProg]
		if p == nil {
			return nil, "", fmt.Errorf("ebpfutil: missing kprobe program %s", h.KprobeProg)
		}
		if l, err := link.Kprobe(h.KprobeSym, p, nil); err == nil {
			return l, "kprobe:" + h.KprobeSym, nil
		} else {
			lastErr = err
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no attach strategy available")
	}
	return nil, "", lastErr
}

// Package ebpfutil holds helpers shared by the eBPF plugins.
package ebpfutil

import (
	"errors"
	"fmt"

	"github.com/cilium/ebpf"
)

// DrainPercpuU64 sums the per-CPU u64 values of each key of a PERCPU_HASH
// map, passes the totals to fn, then deletes the keys (read-then-clear).
//
// Two notes:
//   - LookupAndDelete is not portable to per-CPU maps on older kernels, so
//     this uses lookup + delete; events arriving in between are deleted with
//     the key (a sub-millisecond loss window per 5s window — acceptable for
//     delta accounting).
//   - Iteration and deletion are two phases to avoid mutating mid-scan.
func DrainPercpuU64(m *ebpf.Map, fn func(key uint32, total uint64)) error {
	if m == nil {
		return fmt.Errorf("ebpfutil: nil map")
	}

	var key uint32
	var vals []uint64
	var keys []uint32
	totals := make(map[uint32]uint64)

	iter := m.Iterate()
	for iter.Next(&key, &vals) {
		var total uint64
		for _, v := range vals {
			total += v
		}
		keys = append(keys, key)
		totals[key] = total
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("ebpfutil: iterate: %w", err)
	}

	for _, k := range keys {
		if err := m.Delete(&k); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			return fmt.Errorf("ebpfutil: delete key %d: %w", k, err)
		}
		fn(k, totals[k])
	}
	return nil
}

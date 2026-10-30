// Package ebpfutil holds helpers shared by the eBPF plugins.
package ebpfutil

import (
	"errors"
	"fmt"

	"github.com/cilium/ebpf"
)

// DrainPercpu sums the per-CPU u64 values of each key of a PERCPU_HASH map,
// passes the totals to fn, then deletes the keys (read-then-clear).
//
// Two notes:
//   - LookupAndDelete is not portable to per-CPU maps on older kernels, so
//     this uses lookup + delete; events arriving in between are deleted with
//     the key (a sub-millisecond loss window per 5s window — acceptable for
//     delta accounting).
//   - Iteration and deletion are two phases to avoid mutating mid-scan.
//
// K is the map key type (fixed-size value type, e.g. uint32 or a struct
// mirroring the C key layout).
func DrainPercpu[K comparable](m *ebpf.Map, fn func(key K, total uint64)) error {
	if m == nil {
		return fmt.Errorf("ebpfutil: nil map")
	}

	var key K
	var vals []uint64
	var keys []K
	totals := make(map[K]uint64)

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
			return fmt.Errorf("ebpfutil: delete key: %w", err)
		}
		fn(k, totals[k])
	}
	return nil
}

// DrainPercpuU64 is DrainPercpu for u32-keyed maps.
func DrainPercpuU64(m *ebpf.Map, fn func(key uint32, total uint64)) error {
	return DrainPercpu[uint32](m, fn)
}

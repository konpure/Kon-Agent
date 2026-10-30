package ebpfutil

import (
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// CgroupResolver maps kernel cgroup ids to short human-readable names.
//
// On cgroup v2, the id returned by bpf_get_current_cgroup_id /
// bpf_sk_cgroup_id equals the inode of the cgroup's directory under
// /sys/fs/cgroup, so resolution is a directory walk + stat. Scans are cached
// and re-run at most every rescanInterval (on cache miss).
type CgroupResolver struct {
	mu       sync.Mutex
	byID     map[uint64]string
	lastScan time.Time
	root     string
}

func NewCgroupResolver() *CgroupResolver {
	return &CgroupResolver{
		byID: make(map[uint64]string),
		root: "/sys/fs/cgroup",
	}
}

// Name resolves a cgroup id. 0 means "no association" (host).
func (r *CgroupResolver) Name(id uint64) string {
	if id == 0 {
		return "host"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if name, ok := r.byID[id]; ok {
		return name
	}
	if time.Since(r.lastScan) > 30*time.Second {
		r.scanLocked()
	}
	if name, ok := r.byID[id]; ok {
		return name
	}
	return "unknown"
}

func (r *CgroupResolver) scanLocked() {
	r.lastScan = time.Now()
	r.byID = make(map[uint64]string)
	_ = filepath.WalkDir(r.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		var st syscall.Stat_t
		if err := syscall.Stat(path, &st); err != nil {
			return nil
		}
		if path == r.root {
			r.byID[st.Ino] = "host"
			return nil
		}
		r.byID[st.Ino] = shortCgroupName(path)
		return nil
	})
}

// shortCgroupName compresses a cgroupfs path into a label-safe short name:
// container runtime scopes become the (truncated) container id, k8s pod
// slices the pod uid; everything else keeps its last component.
func shortCgroupName(path string) string {
	base := filepath.Base(path)
	for _, prefix := range []string{"cri-containerd-", "docker-", "crio-", "cri-o-"} {
		if strings.HasPrefix(base, prefix) && strings.HasSuffix(base, ".scope") {
			id := strings.TrimSuffix(strings.TrimPrefix(base, prefix), ".scope")
			if len(id) > 12 {
				id = id[:12]
			}
			return id
		}
	}
	// k8s pod slices look like kubepods-burstable-pod<uid>.slice
	if idx := strings.Index(base, "-pod"); idx >= 0 && strings.HasSuffix(base, ".slice") {
		uid := strings.TrimSuffix(base[idx+4:], ".slice")
		uid = strings.ReplaceAll(uid, "_", "-")
		if len(uid) > 12 {
			uid = uid[:12]
		}
		return "pod/" + uid
	}
	if strings.HasSuffix(base, ".slice") || strings.HasSuffix(base, ".scope") {
		return strings.TrimSuffix(strings.TrimSuffix(base, ".slice"), ".scope")
	}
	return base
}

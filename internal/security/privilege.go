package security

import (
	"errors"
	"fmt"
	"github.com/syndtr/gocapability/capability"
	"golang.org/x/sys/unix"
	"log/slog"
	"syscall"
)

var allowedCaps = []capability.Cap{
	capability.CAP_BPF,
	capability.CAP_NET_RAW,
	capability.CAP_PERFMON,
	capability.CAP_SYS_RESOURCE,
}

func DropPrivileges() error {
	// Get current process capabilities
	caps, err := capability.NewPid2(0)
	if err != nil {
		return fmt.Errorf("failed to get capabilities: %w", err)
	}

	if err := caps.Load(); err != nil {
		return fmt.Errorf("failed to load capabilities: %w", err)
	}

	// Drop all capabilities
	caps.Clear(capability.EFFECTIVE | capability.PERMITTED | capability.INHERITABLE)

	// Set allowed capabilities
	for _, c := range allowedCaps {
		caps.Set(capability.EFFECTIVE|capability.PERMITTED, c)
	}

	// Apply capabilities
	if err := caps.Apply(capability.EFFECTIVE | capability.PERMITTED | capability.INHERITABLE); err != nil {
		return fmt.Errorf("failed to apply capabilities: %v", err)
	}

	// Switch to nobody user
	// if err := syscall.Setuid(65534); err != nil {
	// 	 return fmt.Errorf("failed to set uid to nobody: %v", err)
	// }
	// if err := syscall.Setgid(65534); err != nil {
	// 	 return fmt.Errorf("failed to set gid to nobody: %v", err)
	// }

	if err := syscall.Setpgid(0, 0); err != nil {
		// A session leader cannot change its own process group (EPERM) —
		// expected when running as PID 1 in a container. This is process
		// hygiene, not a privilege boundary, so tolerate only that case;
		// any other failure still surfaces (bare-metal behavior preserved).
		if errors.Is(err, unix.EPERM) {
			slog.Debug("setpgid skipped (session leader, e.g. container PID 1)")
		} else {
			return fmt.Errorf("failed to set pgid: %w", err)
		}
	}

	unix.Umask(0o077)
	return nil

}

package security

import (
	"fmt"
	"github.com/syndtr/gocapability/capability"
	"golang.org/x/sys/unix"
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
	if err := syscall.Setuid(65534); err != nil {
		return fmt.Errorf("failed to set uid to nobody: %v", err)
	}
	if err := syscall.Setgid(65534); err != nil {
		return fmt.Errorf("failed to set gid to nobody: %v", err)
	}

	if err := syscall.Setpgid(0, 0); err != nil {
		return fmt.Errorf("failed to set pgid: %v", err)
	}

	unix.Umask(0o077)
	return nil

}

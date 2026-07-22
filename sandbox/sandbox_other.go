//go:build !linux

package sandbox

import (
	"context"
	"errors"
	"io"
)

// ErrUnsupported is returned by Run on platforms without the rootless jail.
var ErrUnsupported = errors.New("sandbox: unsupported on this platform")

// Mount is a mount to set up inside the new root. FSType=="" means a bind mount
// of Source onto Target; otherwise FSType is a filesystem type ("tmpfs",
// "proc", ...). Target is relative to NewRoot.
type Mount struct {
	Source   string
	Target   string
	FSType   string
	ReadOnly bool
	Flags    uintptr
}

// Namespaces selects which namespaces the child is cloned into.
type Namespaces struct{ User, Mount, PID, Net, UTS, IPC bool }

// Config fully describes one sandboxed command invocation.
type Config struct {
	Command    []string
	Env        []string
	Dir        string
	NewRoot    string
	Mounts     []Mount
	Namespaces Namespaces
	Cgroup     *Cgroup   `json:"-"`
	Stdout     io.Writer `json:"-"`
	Stderr     io.Writer `json:"-"`
	Stdin      io.Reader `json:"-"`
}

// Run is unsupported off Linux.
func Run(ctx context.Context, cfg Config) (int, error) {
	return -1, ErrUnsupported
}

// Init is a no-op off Linux (there is no re-exec child to dispatch).
func Init() {}

// UserNSAvailable always reports false off Linux.
func UserNSAvailable() bool { return false }

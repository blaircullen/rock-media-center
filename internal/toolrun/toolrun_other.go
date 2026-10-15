//go:build !linux

package toolrun

import "os/exec"

// ownGroup is a no-op off Linux: cancelling kills the started process only (exec's
// default), not anything it spawned.
func ownGroup(cmd *exec.Cmd) {}

// lower is a no-op off Linux; tools run at the priority they inherit.
func lower(pgid int, class Class) {}

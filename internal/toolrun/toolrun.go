// Package toolrun starts the helper tools Rock Media Center shells out to (ffprobe today) — the one
// way they should all be started.
//
// A bare exec.CommandContext gets none of what a long-running tool on a shared home server
// needs:
//
//   - Its own process group, killed as a whole when the context ends. CommandContext kills
//     only the process it started; anything that process spawned was left running.
//   - A lower CPU and disk priority (nice 19 and a lower I/O class) when asked for, so Plex
//     and everything else interactive win the machine.
//   - A hook when the process starts and ends, with its process group id.
//
// Linux only, which is the deployment target. On other platforms (Windows and macOS dev
// machines) the class is ignored and cancelling kills only the started process.
//
// API, in short:
//
//	cmd := toolrun.Command(ctx, toolrun.Idle, "ffmpeg", args...)
//	cmd.OnStart = func(pgid int) { ... }   // optional
//	cmd.OnExit = func(pgid int) { ... }    // optional: unregister
//	out, err := cmd.CombinedOutput()       // or Run / Output / Start+Wait, StdoutPipe…
//
// Cmd embeds *exec.Cmd, so Stdin/Stdout/Stderr, Dir, Env and the pipe helpers work as usual;
// only Start, Wait, Run, Output and CombinedOutput are its own (they apply the class and
// call the hooks). Always use those, never the embedded exec.Cmd's.
package toolrun

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

// Class is how much of the machine a tool may take.
type Class int

const (
	// Normal leaves the priority alone. For light, short tools whose answer someone is
	// waiting on: ffprobe, an encoder self-test at startup, a --help probe.
	Normal Class = iota
	// Background is nice 19 with the lowest best-effort I/O level (7). For bulk work that
	// must still make steady progress on a busy disk.
	Background
	// Idle is nice 19 with the idle I/O class: the tool only gets the disk when nothing
	// else wants it. For side reads that can wait.
	Idle
)

func (c Class) String() string {
	switch c {
	case Background:
		return "background"
	case Idle:
		return "idle"
	}
	return "normal"
}

// Nice is the CPU niceness Background and Idle tools run at: the lowest priority there is.
const Nice = 19

// waitDelay bounds how long Wait waits for a tool's output pipes once the tool has exited
// or been killed. Without it a grandchild that kept a pipe open (outside the group) would
// hang Wait for ever; with the group kill that should never happen, so this is a backstop.
const waitDelay = 30 * time.Second

// Cmd is a helper tool started through this package. See the package doc.
type Cmd struct {
	*exec.Cmd
	class Class
	// OnStart, when set, is called right after the tool has started and been lowered to its
	// class, with its process group id (the same as its pid).
	OnStart func(pgid int)
	// OnExit, when set, is called once Wait has returned (with the same id) — only for a
	// tool whose OnStart ran.
	OnExit func(pgid int)
	pgid   int
}

// Command is exec.CommandContext for a helper tool: its own process group, killed as a
// whole when ctx ends, and lowered to class once started.
func Command(ctx context.Context, class Class, name string, args ...string) *Cmd {
	c := &Cmd{Cmd: exec.CommandContext(ctx, name, args...), class: class}
	c.WaitDelay = waitDelay
	ownGroup(c.Cmd)
	return c
}

// Start starts the tool, lowers it to its class and calls OnStart.
func (c *Cmd) Start() error {
	if err := c.Cmd.Start(); err != nil {
		return err
	}
	c.pgid = c.Process.Pid
	lower(c.pgid, c.class)
	if c.OnStart != nil {
		c.OnStart(c.pgid)
	}
	return nil
}

// Wait waits for the tool to exit and calls OnExit.
func (c *Cmd) Wait() error {
	err := c.Cmd.Wait()
	if c.OnExit != nil && c.pgid != 0 {
		c.OnExit(c.pgid)
	}
	return err
}

// Run starts the tool and waits for it.
func (c *Cmd) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

// Output runs the tool and returns its standard output. Like exec.Cmd.Output, a failure's
// standard error is kept on the *exec.ExitError when Stderr wasn't set.
func (c *Cmd) Output() ([]byte, error) {
	if c.Stdout != nil {
		return nil, errors.New("toolrun: Stdout already set")
	}
	var out bytes.Buffer
	c.Stdout = &out
	var stderr *bytes.Buffer
	if c.Stderr == nil {
		stderr = &bytes.Buffer{}
		c.Stderr = stderr
	}
	err := c.Run()
	var ee *exec.ExitError
	if stderr != nil && errors.As(err, &ee) {
		ee.Stderr = stderr.Bytes()
	}
	return out.Bytes(), err
}

// CombinedOutput runs the tool and returns its standard output and error together.
func (c *Cmd) CombinedOutput() ([]byte, error) {
	if c.Stdout != nil || c.Stderr != nil {
		return nil, errors.New("toolrun: Stdout or Stderr already set")
	}
	var b bytes.Buffer
	c.Stdout, c.Stderr = &b, &b
	err := c.Run()
	return b.Bytes(), err
}

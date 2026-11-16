// Package executor runs shell scripts on a server.
//
// Everything Skiff does to a server goes through the Executor interface. Today
// the only real implementation runs commands on the machine Skiff is installed
// on; a central multi-server panel can add an SSH (or agent) implementation
// without touching provisioning or deployment code.
package executor

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"syscall"
	"time"
)

type Options struct {
	User  string    // run as this system user; "" means the panel's own user
	Dir   string    // working directory
	Env   []string  // extra KEY=VALUE pairs
	Stdin io.Reader // optional input, e.g. file contents to write
}

type Executor interface {
	// Run executes a bash script, streaming stdout and stderr to out.
	Run(ctx context.Context, script string, opt Options, out io.Writer) error
}

// Local runs scripts on this machine with bash.
type Local struct{}

func (Local) Run(ctx context.Context, script string, opt Options, out io.Writer) error {
	name, args := "bash", []string{"-c", script}
	if opt.User != "" && !isCurrentUser(opt.User) {
		// runuser keeps the environment (so Env reaches the script) but sets
		// HOME/USER for the target user. Requires root, which Skiff runs as.
		name, args = "runuser", append([]string{"-u", opt.User, "--", "bash"}, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = opt.Dir
	cmd.Env = append(os.Environ(), opt.Env...)
	cmd.Stdin = opt.Stdin
	cmd.Stdout, cmd.Stderr = out, out
	// Kill the whole process group on cancel so composer & friends don't linger.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

func isCurrentUser(name string) bool {
	u, err := user.Current()
	return err == nil && u.Username == name
}

// DryRun prints scripts instead of running them. Useful for developing the UI
// on a laptop that isn't an Ubuntu server.
type DryRun struct{}

func (DryRun) Run(ctx context.Context, script string, opt Options, out io.Writer) error {
	who := opt.User
	if who == "" {
		who = "(panel user)"
	}
	fmt.Fprintf(out, "[dry-run] as %s", who)
	if opt.Dir != "" {
		fmt.Fprintf(out, " in %s", opt.Dir)
	}
	fmt.Fprintln(out)
	for _, line := range strings.Split(strings.TrimRight(script, "\n"), "\n") {
		fmt.Fprintf(out, "  | %s\n", line)
	}
	if opt.Stdin != nil {
		n, _ := io.Copy(io.Discard, opt.Stdin)
		fmt.Fprintf(out, "  (stdin: %d bytes)\n", n)
	}
	time.Sleep(150 * time.Millisecond) // make the live log visibly stream
	return ctx.Err()
}

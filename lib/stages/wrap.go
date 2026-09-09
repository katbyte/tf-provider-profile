package stages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// Wrap execs a provider binary on terraform's behalf, passing stdio straight through so the go-plugin handshake is
// untouched, then appends the child's peak RSS (bytes) to rssOut once it exits. It is installed as the dev_overrides
// plugin executable by the schema stage. Returns the exit code to use.
func Wrap(rssOut string, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "tfpp _wrap: missing provider binary")
		return 2
	}
	cmd := exec.CommandContext(context.Background(), args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "tfpp _wrap: starting %s: %v\n", args[0], err)
		return 2
	}

	// forward termination signals so terraform killing us kills the provider
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				_ = cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()

	werr := cmd.Wait()
	close(done)
	signal.Stop(sigs)

	if f, err := os.OpenFile(rssOut, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_, _ = fmt.Fprintf(f, "%d\n", maxRSSBytes(cmd.ProcessState))
		_ = f.Close()
	}

	if ee, ok := errors.AsType[*exec.ExitError](werr); ok {
		return ee.ExitCode()
	}
	if werr != nil {
		return 2
	}
	return 0
}

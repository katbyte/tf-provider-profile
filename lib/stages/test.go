package stages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/katbyte/tf-provider-profile/lib/results"
)

// per-package result lines only: the bare FAIL/ok summary lines have no trailing tab
var (
	reTestOK   = regexp.MustCompile(`(?m)^ok[ \t]`)
	reTestFail = regexp.MustCompile(`(?m)^FAIL[ \t]`)
)

// testStage times the provider's unit tests at the tag: `make test` when the provider defines the target (its own
// definition of a unit test run), plain `go test ./...` otherwise. Failing tests at a tag are data, not a broken
// stage, so the timing is recorded with the exit code as long as any package actually ran.
func (r *Runner) testStage(ctx context.Context, res *results.Result) (string, error) {
	if _, err := r.checkoutTag(ctx, res.Version); err != nil {
		return "", err
	}
	src := r.P.SrcDir()
	env := append(r.buildEnv(src), r.tfEnv()...)

	name, args, tool := "go", []string{"test", "./..."}, "go test"
	if makeTarget(src, "test") {
		name, args, tool = "make", []string{"test"}, "make test"
	}

	// resolve (and if needed download) the toolchain before the clock starts, then empty the build cache so every
	// release is timed from cold like the build stage does: compiling this release's tree is part of what a test run
	// costs, and a cache shared across a sweep otherwise grows without bound (300GB of it filled a disk)
	if _, err := runCmd(ctx, src, env, "go", "version"); err != nil {
		return "", err
	}
	if _, err := runCmd(ctx, src, env, "go", "clean", "-cache"); err != nil {
		return "", err
	}

	timeout := r.Opts.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Minute
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(tctx, name, args...)
	cmd.Dir = src
	cmd.Env = append(os.Environ(), env...)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start).Seconds()

	tr := &results.TestResult{
		Tool:      tool,
		DurationS: elapsed,
		Packages:  len(reTestOK.FindAll(out, -1)) + len(reTestFail.FindAll(out, -1)),
		Failures:  len(reTestFail.FindAll(out, -1)),
	}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return "", err
		}
		tr.ExitCode = ee.ExitCode()
		// no package results means the run never really started (compile error, missing tool)
		if tr.Packages == 0 {
			return "", fmt.Errorf("%s exit %d after %.0fs: %s", tool, tr.ExitCode, elapsed, lastLines(string(out), 6))
		}
	}

	res.Test = tr
	return fmt.Sprintf("%.0fs pkgs %d failures %d exit %d (%s)", tr.DurationS, tr.Packages, tr.Failures, tr.ExitCode, tool), nil
}

// tfEnv points tests built on helper/resource at the pinned terraform via TF_ACC_TERRAFORM_PATH, so they never
// download a CLI mid-run (which drags release-key expiry and network variance into the timing).
func (r *Runner) tfEnv() []string {
	if r.Opts.Terraform == "" {
		return nil
	}
	tf, err := exec.LookPath(r.Opts.Terraform)
	if err != nil {
		return nil
	}
	abs, err := filepath.Abs(tf)
	if err != nil {
		return nil
	}
	return []string{"TF_ACC_TERRAFORM_PATH=" + abs}
}

// makeTarget reports whether the checkout's makefile defines the given target.
func makeTarget(src, target string) bool {
	for _, f := range []string{"GNUmakefile", "Makefile", "makefile"} {
		if b, err := os.ReadFile(filepath.Join(src, f)); err == nil {
			if regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(target) + `:`).Match(b) {
				return true
			}
		}
	}
	return false
}

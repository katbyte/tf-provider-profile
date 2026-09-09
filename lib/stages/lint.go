package stages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/katbyte/tf-provider-profile/lib/clog"
	"github.com/katbyte/tf-provider-profile/lib/results"
)

var reLintIssue = regexp.MustCompile(`(?m)^\S+\.go:\d+:\d+: `)

// lintStage times a cold golangci-lint run at the tag. Providers that build a custom golangci binary with module
// plugins (azurerm: `make golangci-with-modules` -> scripts/golangci-with-modules) get that binary built untimed
// first and then timed, since stock golangci-lint rejects their config; otherwise the golangci-lint on PATH is used.
func (r *Runner) lintStage(ctx context.Context, res *results.Result) (string, error) {
	if _, err := r.checkoutTag(ctx, res.Version); err != nil {
		return "", err
	}
	src := r.P.SrcDir()
	env := append(r.buildEnv(src), "GOLANGCI_LINT_CACHE="+filepath.Join(r.P.CacheDir, "golangci-cache"))

	tool := ""
	if _, err := os.Stat(filepath.Join(src, "scripts", ".custom-gcl.yml")); err == nil {
		if _, err := runCmd(ctx, src, env, "make", "golangci-with-modules"); err != nil {
			return "", fmt.Errorf("building custom golangci-lint: %w", err)
		}
		tool = filepath.Join(src, "scripts", "golangci-with-modules")
	}
	if tool == "" {
		name := r.Opts.GolangciLint
		if name == "" {
			name = "golangci-lint"
		}
		if strings.Contains(name, string(filepath.Separator)) {
			name, _ = filepath.Abs(name) // the command runs with cmd.Dir set to the checkout
		}
		p, err := exec.LookPath(name)
		if err != nil {
			return "", fmt.Errorf("golangci-lint not found: %w", err)
		}
		tool = p
	}

	ver, _ := runCmd(ctx, src, env, tool, "version", "--short")
	// providers still on a v1 config cannot be linted by golangci-lint v2 as-is; migrate the checkout's copy in place
	// (the next checkout --force discards it) so the run measures the same rule set the provider intended
	if cfg, err := os.ReadFile(filepath.Join(src, ".golangci.yml")); err == nil && !strings.Contains(string(cfg), "version: \"2\"") && !strings.Contains(string(cfg), "version: '2'") && !strings.Contains(string(cfg), "version: 2") {
		if _, err := runCmd(ctx, src, env, tool, "migrate", "--skip-validation"); err != nil {
			clog.Log.Warnf("%s: golangci-lint migrate failed, running with the original config: %v", res.Version, err)
		}
	}
	if _, err := runCmd(ctx, src, env, tool, "cache", "clean"); err != nil {
		return "", err
	}

	timeout := r.Opts.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Minute
	}
	lctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(lctx, tool, "run", "./...", "--timeout", timeout.String())
	cmd.Dir = src
	cmd.Env = append(os.Environ(), env...)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start).Seconds()

	lr := &results.LintResult{
		Tool:        filepath.Base(tool),
		ToolVersion: strings.TrimSpace(ver),
		DurationS:   elapsed,
		Issues:      len(reLintIssue.FindAllIndex(out, -1)),
	}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return "", err
		}
		lr.ExitCode = ee.ExitCode()
		// 1 = issues found, which is a valid timing; anything else is a broken run
		if lr.ExitCode != 1 {
			return "", fmt.Errorf("%s exit %d after %.0fs: %s", lr.Tool, lr.ExitCode, elapsed, lastLines(string(out), 6))
		}
	}

	res.Lint = lr
	return fmt.Sprintf("%.0fs issues %d exit %d (%s %s)", lr.DurationS, lr.Issues, lr.ExitCode, lr.Tool, lr.ToolVersion), nil
}

package stages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/katbyte/tf-provider-profile/lib/clog"
	"github.com/katbyte/tf-provider-profile/lib/results"
)

// prcheckStage times the provider's aggregate PR checks target (`make pr-check`), the closest measure of what CI
// costs on every PR. Releases without the target record a successful stage with no result. Tools the target needs
// are installed untimed first via `make tools` (into an isolated GOBIN so per-release versions never land in the
// user's GOPATH), kept out of the timing like the custom golangci build in the lint stage.
func (r *Runner) prcheckStage(ctx context.Context, res *results.Result) (string, error) {
	if _, err := r.checkoutTag(ctx, res.Version); err != nil {
		return "", err
	}
	src := r.P.SrcDir()
	if !makeTarget(src, "pr-check") {
		return "no pr-check target", nil
	}

	env := append(r.buildEnv(src), "GOLANGCI_LINT_CACHE="+filepath.Join(r.P.CacheDir, "golangci-cache"))
	// `go install tool@version` in make tools refuses to run with -mod=vendor forced; drop the flag and let go's
	// auto-vendoring keep the actual builds on the vendor dir
	env = append(env, "GOFLAGS=")
	env = append(env, r.tfEnv()...)
	// tool installs (`go install` and `curl ... | sh -b $(go env GOPATH)/bin` alike) land in an isolated GOPATH so
	// per-release versions never touch the user's; the real module cache is shared so nothing re-downloads
	gopath := filepath.Join(r.P.CacheDir, "gopath")
	if mc, err := runCmd(ctx, src, nil, "go", "env", "GOMODCACHE"); err == nil && strings.TrimSpace(mc) != "" {
		env = append(env, "GOMODCACHE="+strings.TrimSpace(mc))
	}
	env = append(env, "GOPATH="+gopath, "PATH="+filepath.Join(gopath, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	if makeTarget(src, "tools") {
		if _, err := runCmd(ctx, src, env, "make", "tools"); err != nil {
			return "", fmt.Errorf("installing tools: %w", err)
		}
	}

	timeout := r.Opts.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Hour
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// -k keeps the remaining sub-targets running past a failure, so the duration is the full gate cost even when a
	// release has genuine findings at its tag (exit code and fail tail still record that it failed)
	cmd := exec.CommandContext(pctx, "make", "-k", "pr-check")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), env...)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start).Seconds()

	pr := &results.PRCheckResult{Target: "pr-check", DurationS: elapsed}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return "", err
		}
		// a failing pr-check is how long CI takes to say no, which is still the number being measured
		pr.ExitCode = ee.ExitCode()
		pr.FailTail = lastLines(string(out), 6)
		clog.Log.Debugf("%s: make pr-check exit %d: %s", res.Version, pr.ExitCode, pr.FailTail)
	}

	res.PRCheck = pr
	return fmt.Sprintf("%.0fs exit %d (make pr-check)", pr.DurationS, pr.ExitCode), nil
}

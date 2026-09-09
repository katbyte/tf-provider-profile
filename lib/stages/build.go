package stages

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/katbyte/tf-provider-profile/lib/results"
)

// buildEnv returns the environment for go commands in the source tree: an isolated GOCACHE so runs are comparable
// and never pollute the user's cache, vendor mode when a vendor dir exists, and the toolchain the release pins.
func (r *Runner) buildEnv(src string) []string {
	env := []string{
		"GOCACHE=" + r.P.GoCacheDir(),
		"CGO_ENABLED=0",
		"GOFLAGS=",
	}
	if _, err := os.Stat(filepath.Join(src, "vendor")); err == nil {
		env = append(env, "GOFLAGS=-mod=vendor")
	}
	if b, err := os.ReadFile(filepath.Join(src, ".go-version")); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			env = append(env, "GOTOOLCHAIN=go"+v+"+auto")
		}
	}
	return env
}

// buildStage times a clean build (isolated, emptied GOCACHE), a warm rebuild (effectively link time), and a
// stripped build, recording the local binary sizes alongside.
func (r *Runner) buildStage(ctx context.Context, res *results.Result) (string, error) {
	if _, err := r.checkoutTag(ctx, res.Version); err != nil {
		return "", err
	}
	src := r.P.SrcDir()
	env := r.buildEnv(src)
	outDir := filepath.Join(r.P.BuildOutDir(), res.Version)
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(outDir) }() // 200MB+ each; only the sizes are kept

	// resolve (and if needed download) the toolchain before the clock starts
	out, err := runCmd(ctx, src, env, "go", "version")
	if err != nil {
		return "", err
	}
	br := &results.BuildResult{GoToolchain: strings.TrimSpace(strings.TrimPrefix(out, "go version "))}

	if _, err := runCmd(ctx, src, env, "go", "clean", "-cache"); err != nil {
		return "", err
	}

	bin := filepath.Join(outDir, r.P.BinaryName())
	timeBuild := func(args ...string) (float64, int64, error) {
		_ = os.Remove(bin)
		start := time.Now()
		if _, err := runCmd(ctx, src, env, "go", append([]string{"build", "-o", bin}, append(args, ".")...)...); err != nil {
			return 0, 0, err
		}
		elapsed := time.Since(start).Seconds()
		st, serr := os.Stat(bin)
		if serr != nil {
			return 0, 0, serr
		}
		return elapsed, st.Size(), nil
	}

	if br.CleanBuildS, br.LocalBinaryBytes, err = timeBuild(); err != nil {
		return "", fmt.Errorf("clean build: %w", err)
	}
	if br.WarmBuildS, _, err = timeBuild(); err != nil {
		return "", fmt.Errorf("warm build: %w", err)
	}
	if br.StrippedBuildS, br.StrippedBinaryBytes, err = timeBuild("-ldflags=-s -w"); err != nil {
		return "", fmt.Errorf("stripped build: %w", err)
	}

	res.Build = br
	return fmt.Sprintf("clean %.0fs warm %.0fs stripped %.0fs; local %s stripped %s (%s)",
		br.CleanBuildS, br.WarmBuildS, br.StrippedBuildS, humanBytes(br.LocalBinaryBytes), humanBytes(br.StrippedBinaryBytes), br.GoToolchain), nil
}

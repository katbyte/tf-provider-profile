// Package stages implements the profiling stages tfpp runs against each provider release: download the release
// zips, dissect the binaries, time the plugin handshake and terraform schema fetch, count things in the source tree
// at the tag, and (sampled) time a clean build, the unit tests, a lint run, and the provider's pr-check target.
package stages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/katbyte/tf-provider-profile/lib/clog"
	"github.com/katbyte/tf-provider-profile/lib/cout"
	"github.com/katbyte/tf-provider-profile/lib/provider"
	"github.com/katbyte/tf-provider-profile/lib/results"
)

// Options controls a profiling run.
type Options struct {
	Platforms    []string // platforms to download/dissect; timing stages use the native one
	Stages       []string // stages to run, in results.AllStages order
	Force        bool     // rerun stages that already completed
	Retry        bool     // rerun stages that previously failed
	Concurrency  int      // parallel downloads
	Runs         int      // repetitions for timing stages (startup, schema)
	Terraform    string   // terraform binary for the schema stage
	GolangciLint string   // golangci-lint binary for the lint stage fallback
	KeepZips     bool     // keep zips after extraction (needed for zip size; default true)
	Timeout      time.Duration
}

// Runner executes stages for a provider.
type Runner struct {
	P     *provider.Provider
	Store *results.Store
	Opts  Options
	All   []provider.Release // every known release (for previous-version lookups)
}

// Run profiles the given releases. sampled is the subset of releases the expensive stages run for.
func (r *Runner) Run(ctx context.Context, rels, sampled []provider.Release) error {
	sampledSet := map[string]bool{}
	for _, s := range sampled {
		sampledSet[s.Version] = true
	}

	native := provider.NativePlatform()
	if !slices.Contains(r.Opts.Platforms, native) {
		cout.Printf("<yellow>note:</> native platform %s not in --platforms; startup and schema stages will be skipped\n", native)
	}

	// downloads first, in parallel, so the sequential timing stages never wait on the network; only releases with
	// pending work are fetched so a fresh machine with committed results does not pull every zip again
	if r.want(results.StageDownload) {
		var pending []provider.Release
		for _, rel := range rels {
			res, err := r.Store.Load(rel)
			if err != nil {
				return err
			}
			if r.hasPendingWork(res, sampledSet[rel.Version]) {
				pending = append(pending, rel)
			}
		}
		if err := r.downloadAll(ctx, pending); err != nil {
			return err
		}
	}

	for i, rel := range rels {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cout.Printf("\n<white>==></> <cyan>%s</> <gray>(%s, %d/%d)</>\n", rel.Version, rel.Date.Format("2006-01-02"), i+1, len(rels))

		res, err := r.Store.Load(rel)
		if err != nil {
			return err
		}

		for _, stage := range results.AllStages {
			if !r.want(stage) || stage == results.StageDownload {
				continue
			}
			if slices.Contains(results.ExpensiveStages, stage) && !sampledSet[rel.Version] {
				cout.Verbosef("  <gray>%-9s not sampled</>\n", stage)
				continue
			}
			if !r.shouldRun(res, stage) {
				cout.Printf("  <gray>%-9s already done</>\n", stage)
				continue
			}

			r.runStage(ctx, res, stage)
			if err := r.Store.Save(res); err != nil {
				return err
			}
		}
	}

	return nil
}

func (r *Runner) want(stage string) bool { return slices.Contains(r.Opts.Stages, stage) }

// hasPendingWork reports whether any requested non-download stage would run for this release.
func (r *Runner) hasPendingWork(res *results.Result, sampled bool) bool {
	for _, stage := range results.AllStages {
		if stage == results.StageDownload || !r.want(stage) {
			continue
		}
		if slices.Contains(results.ExpensiveStages, stage) && !sampled {
			continue
		}
		if r.shouldRun(res, stage) {
			return true
		}
	}
	// download alone requested: fetch everything asked for
	return len(r.Opts.Stages) == 1 && r.Opts.Stages[0] == results.StageDownload
}

func (r *Runner) shouldRun(res *results.Result, stage string) bool {
	if r.Opts.Force {
		return true
	}
	if res.Failed(stage) {
		return r.Opts.Retry
	}
	return !res.Done(stage)
}

func (r *Runner) runStage(ctx context.Context, res *results.Result, stage string) {
	cout.Printf("  <white>%-9s</> ", stage)
	start := time.Now()
	host, _ := os.Hostname()

	var err error
	var summary string
	switch stage {
	case results.StageBinary:
		summary, err = r.binaryStage(ctx, res)
	case results.StageStartup:
		summary, err = r.startupStage(ctx, res)
	case results.StageSchema:
		summary, err = r.schemaStage(ctx, res)
	case results.StageSource:
		summary, err = r.sourceStage(ctx, res)
	case results.StageBuild:
		summary, err = r.buildStage(ctx, res)
	case results.StageTest:
		summary, err = r.testStage(ctx, res)
	case results.StageLint:
		summary, err = r.lintStage(ctx, res)
	case results.StagePRCheck:
		summary, err = r.prcheckStage(ctx, res)
	default:
		err = fmt.Errorf("unknown stage %q", stage)
	}

	meta := results.StageMeta{RanAt: start.UTC(), DurationS: time.Since(start).Seconds(), Host: host, CPU: cpuModel(), OS: runtime.GOOS + "/" + runtime.GOARCH}
	if err != nil {
		meta.Error = err.Error()
		cout.Printf("<red>failed</> <gray>(%.1fs)</> %v\n", meta.DurationS, err)
		clog.Log.Debugf("%s %s: %v", res.Version, stage, err)
	} else {
		cout.Printf("%s <gray>(%.1fs)</>\n", summary, meta.DurationS)
	}
	res.Stages[stage] = meta
}

// prevRelease returns the release published before v, if known.
func (r *Runner) prevRelease(v string) (provider.Release, bool) {
	all := slices.Clone(r.All)
	provider.SortReleases(all)
	for i, rel := range all {
		if rel.Version == v && i > 0 {
			return all[i-1], true
		}
	}
	return provider.Release{}, false
}

// runCmd runs a command capturing combined output, returning it with a descriptive error on failure.
func runCmd(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	s := string(out)
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return s, fmt.Errorf("%s %s: exit %d: %s", name, strings.Join(args, " "), ee.ExitCode(), lastLines(s, 5))
		}
		return s, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return s, nil
}

var cpuModelOnce = sync.OnceValue(func() string {
	switch runtime.GOOS {
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			return strings.TrimSpace(string(out))
		}
	case "linux":
		if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for line := range strings.SplitSeq(string(b), "\n") {
				if name, val, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(name) == "model name" {
					return strings.TrimSpace(val)
				}
			}
		}
	}
	return ""
})

func cpuModel() string { return cpuModelOnce() }

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[len(s)/2]
}

func minOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	return slices.Min(xs)
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTPE"[exp])
}

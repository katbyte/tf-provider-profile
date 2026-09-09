package stages

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/katbyte/tf-provider-profile/lib/provider"
	"github.com/katbyte/tf-provider-profile/lib/results"
)

// go-plugin handshake: the provider only starts when the parent presents the magic cookie.
const (
	pluginMagicCookieKey   = "TF_PLUGIN_MAGIC_COOKIE"
	pluginMagicCookieValue = "d602bf8f470bc67ca7faa0386276bbdd4330efaf76d1a219cb4d6991ca9872b2"
)

// handshake lines look like: 1|5|unix|/tmp/plugin123|grpc|
var handshakeRe = regexp.MustCompile(`^(\d+)\|(\d+)\|(\w+)\|([^|]+)\|(\w+)\|`)

// GODEBUG=inittrace=1 line: init <pkg> @<start> ms, <clock> ms clock, <bytes> bytes, <allocs> allocs
var initTraceRe = regexp.MustCompile(`^init (\S+) @[\d.]+ ms, ([\d.]+) ms clock, (\d+) bytes, (\d+) allocs`)

// startupStage runs the native binary as a plugin (no terraform) and times how long it takes to print the go-plugin
// handshake line, then kills it and reads its peak RSS. This is the fixed cost of every provider launch.
func (r *Runner) startupStage(ctx context.Context, res *results.Result) (string, error) {
	native := provider.NativePlatform()
	bin, err := r.binaryPath(res.Version, native)
	if err != nil {
		return "", fmt.Errorf("native (%s) binary not downloaded: %w", native, err)
	}

	runs := max(r.Opts.Runs, 1)
	var times, rss []float64
	var proto string
	for range runs {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		ms, rssB, p, err := handshakeOnce(ctx, bin, r.Opts.Timeout)
		if err != nil {
			return "", err
		}
		times = append(times, ms)
		rss = append(rss, float64(rssB))
		proto = p
	}

	res.Startup = &results.StartupResult{
		Platform:        native,
		Runs:            runs,
		HandshakeMinMs:  minOf(times),
		HandshakeMedMs:  median(times),
		MaxRSSBytes:     int64(median(rss)),
		ProtocolVersion: proto,
	}

	// package init cost: three traced runs, keep the cheapest (init time is noisy, allocation counts are not)
	var best *results.StartupResult
	for range min(runs, 3) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		tr, err := initTraceOnce(ctx, bin, r.Opts.Timeout)
		if err != nil {
			return "", err
		}
		if best == nil || tr.InitMs < best.InitMs {
			best = tr
		}
	}
	if best != nil {
		res.Startup.InitPackages, res.Startup.InitMs, res.Startup.InitHeapBytes, res.Startup.InitAllocs, res.Startup.InitTop =
			best.InitPackages, best.InitMs, best.InitHeapBytes, best.InitAllocs, best.InitTop
	}
	return fmt.Sprintf("handshake %.0fms (min %.0fms) rss %s proto %s init %.0fms/%s", res.Startup.HandshakeMedMs, res.Startup.HandshakeMinMs, humanBytes(res.Startup.MaxRSSBytes), proto, res.Startup.InitMs, humanBytes(res.Startup.InitHeapBytes)), nil
}

// initTraceOnce launches the provider with GODEBUG=inittrace=1 and sums the per-package init lines the runtime prints
// to stderr before main; the process is killed once it handshakes.
func initTraceOnce(ctx context.Context, bin string, timeout time.Duration) (*results.StartupResult, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = []string{
		pluginMagicCookieKey + "=" + pluginMagicCookieValue,
		"PLUGIN_PROTOCOL_VERSIONS=5,6",
		"PATH=/usr/bin:/bin",
		"HOME=/tmp",
		"TMPDIR=/tmp",
		"GODEBUG=inittrace=1",
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", bin, err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if handshakeRe.MatchString(sc.Text()) {
			break
		}
	}
	_ = cmd.Process.Kill()
	_, _ = io.Copy(io.Discard, stdout)
	_ = cmd.Wait()

	out := &results.StartupResult{}
	var entries []results.InitEntry
	for line := range strings.SplitSeq(stderr.String(), "\n") {
		m := initTraceRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ms, _ := strconv.ParseFloat(m[2], 64)
		b, _ := strconv.ParseInt(m[3], 10, 64)
		a, _ := strconv.ParseInt(m[4], 10, 64)
		out.InitPackages++
		out.InitMs += ms
		out.InitHeapBytes += b
		out.InitAllocs += a
		entries = append(entries, results.InitEntry{Package: m[1], Ms: ms, Bytes: b})
	}
	if out.InitPackages == 0 {
		return nil, errors.New("no inittrace output on stderr")
	}
	slices.SortFunc(entries, func(a, b results.InitEntry) int {
		return cmp.Or(cmp.Compare(b.Ms, a.Ms), strings.Compare(a.Package, b.Package))
	})
	out.InitTop = entries[:min(10, len(entries))]
	return out, nil
}

func handshakeOnce(ctx context.Context, bin string, timeout time.Duration) (ms float64, rssBytes int64, proto string, err error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = []string{
		pluginMagicCookieKey + "=" + pluginMagicCookieValue,
		"PLUGIN_PROTOCOL_VERSIONS=5,6",
		"PATH=/usr/bin:/bin",
		"HOME=/tmp",
		"TMPDIR=/tmp",
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, 0, "", err
	}
	cmd.Stderr = io.Discard

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return 0, 0, "", fmt.Errorf("starting %s: %w", bin, err)
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	found := false
	for sc.Scan() {
		if m := handshakeRe.FindStringSubmatch(sc.Text()); m != nil {
			ms = float64(time.Since(start).Microseconds()) / 1000
			proto = m[2]
			found = true
			break
		}
	}
	_ = cmd.Process.Kill()
	// drain so the child can exit, then collect rusage
	_, _ = io.Copy(io.Discard, stdout)
	werr := cmd.Wait()
	rssBytes = maxRSSBytes(cmd.ProcessState)

	if !found {
		if ctx.Err() != nil {
			return 0, 0, "", fmt.Errorf("no handshake within %s", timeout)
		}
		if ee, ok := errors.AsType[*exec.ExitError](werr); ok && !strings.Contains(ee.String(), "killed") {
			return 0, 0, "", fmt.Errorf("provider exited before handshake: %w", ee)
		}
		return 0, 0, "", errors.New("provider exited before handshake")
	}
	return ms, rssBytes, proto, nil
}

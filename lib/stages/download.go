package stages

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/katbyte/tf-provider-profile/lib/cout"
	"github.com/katbyte/tf-provider-profile/lib/provider"
)

// downloadAll fetches and extracts every missing release zip for every platform, with bounded concurrency.
func (r *Runner) downloadAll(ctx context.Context, rels []provider.Release) error {
	type job struct {
		rel      provider.Release
		platform string
	}
	var jobs []job
	for _, rel := range rels {
		for _, p := range r.Opts.Platforms {
			if _, err := r.binaryPath(rel.Version, p); err != nil {
				jobs = append(jobs, job{rel, p})
			}
		}
	}
	if len(jobs) == 0 {
		cout.Printf("<white>==></> all release binaries already downloaded\n")
		return nil
	}
	cout.Printf("<white>==></> downloading <yellow>%d</> release zips\n", len(jobs))

	conc := max(r.Opts.Concurrency, 1)
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for _, j := range jobs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			start := time.Now()
			size, err := r.download(ctx, j.rel.Version, j.platform)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				cout.Printf("  <red>%s %s failed:</> %v\n", j.rel.Version, j.platform, err)
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			cout.Printf("  %-9s %-13s %8s <gray>(%.1fs)</>\n", j.rel.Version, j.platform, humanBytes(size), time.Since(start).Seconds())
		})
	}
	wg.Wait()
	return firstErr
}

// zipPath is where a release zip is stored.
func (r *Runner) zipPath(version, platform string) string {
	return filepath.Join(r.P.ReleaseDir(version), r.P.ZipName(version, platform))
}

// binaryPath locates the extracted provider executable for a release/platform.
func (r *Runner) binaryPath(version, platform string) (string, error) {
	dir := filepath.Join(r.P.ReleaseDir(version), platform)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), r.P.BinaryName()) {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("no %s* executable in %s", r.P.BinaryName(), dir)
}

// download fetches one zip (if missing) and extracts it. Returns the zip size.
func (r *Runner) download(ctx context.Context, version, platform string) (int64, error) {
	zp := r.zipPath(version, platform)
	if err := os.MkdirAll(filepath.Dir(zp), 0o750); err != nil {
		return 0, err
	}

	if _, err := os.Stat(zp); errors.Is(err, os.ErrNotExist) {
		url := r.P.ZipURL(version, platform)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
		if err != nil {
			return 0, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("GET %s: %s", url, resp.Status)
		}

		tmp := zp + ".part"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return 0, err
		}
		if _, err := io.Copy(f, resp.Body); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return 0, fmt.Errorf("downloading %s: %w", url, err)
		}
		if err := f.Close(); err != nil {
			return 0, err
		}
		if err := os.Rename(tmp, zp); err != nil {
			return 0, err
		}
	}

	st, err := os.Stat(zp)
	if err != nil {
		return 0, err
	}

	if err := extractZip(zp, filepath.Join(r.P.ReleaseDir(version), platform)); err != nil {
		return 0, fmt.Errorf("extracting %s: %w", zp, err)
	}
	return st.Size(), nil
}

func extractZip(zp, dest string) error {
	zr, err := zip.OpenReader(zp)
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()

	if err := os.MkdirAll(dest, 0o750); err != nil {
		return err
	}
	for _, f := range zr.File {
		// release zips are flat; refuse anything that would escape dest
		name := filepath.Base(f.Name)
		if f.FileInfo().IsDir() || name == "." || name == ".." {
			continue
		}
		out := filepath.Join(dest, name)
		rc, err := f.Open()
		if err != nil {
			return err
		}
		w, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode()|0o600)
		if err != nil {
			_ = rc.Close()
			return err
		}
		_, cerr := io.Copy(w, rc) //nolint:gosec // G110: release zips are a single ~200MB binary, decompression bomb risk is not a concern
		_ = rc.Close()
		if err := w.Close(); err != nil {
			return err
		}
		if cerr != nil {
			return cerr
		}
	}
	return nil
}

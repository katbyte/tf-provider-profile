package stages

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/katbyte/tf-provider-profile/lib/provider"
	"github.com/katbyte/tf-provider-profile/lib/results"
)

// schemaStage runs `terraform providers schema -json` against the release via a dev_overrides config that points at a
// wrapper script, so terraform launches the real provider through `tfpp _wrap` and we get the provider's own peak
// RSS rather than terraform's. Records wall time, provider RSS, schema json size and what the schema contains.
func (r *Runner) schemaStage(ctx context.Context, res *results.Result) (string, error) {
	native := provider.NativePlatform()
	bin, err := r.binaryPath(res.Version, native)
	if err != nil {
		return "", fmt.Errorf("native (%s) binary not downloaded: %w", native, err)
	}
	tf := r.Opts.Terraform
	if tf == "" {
		tf = "terraform"
	}
	tfPath, err := exec.LookPath(tf)
	if err != nil {
		return "", fmt.Errorf("terraform not found: %w", err)
	}
	// the command runs from the per-release schema dir, so a relative --terraform must be resolved first
	if tfPath, err = filepath.Abs(tfPath); err != nil {
		return "", err
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}

	dir := filepath.Join(r.P.SchemaDir(), res.Version)
	pluginDir := filepath.Join(dir, "plugin")
	if err := os.MkdirAll(pluginDir, 0o750); err != nil {
		return "", err
	}
	rssFile := filepath.Join(dir, "rss.txt")
	_ = os.Remove(rssFile)

	files := map[string]string{
		filepath.Join(dir, "main.tf"):              fmt.Sprintf("terraform {\n  required_providers {\n    %s = { source = %q }\n  }\n}\n", r.P.Name, r.P.Source),
		filepath.Join(dir, "terraformrc"):          fmt.Sprintf("provider_installation {\n  dev_overrides { %q = %q }\n  direct {}\n}\n", r.P.Source, pluginDir),
		filepath.Join(pluginDir, r.P.BinaryName()): fmt.Sprintf("#!/bin/sh\nexec %q _wrap --rss-out %q -- %q \"$@\"\n", self, rssFile, bin),
	}
	for p, content := range files {
		mode := os.FileMode(0o600)
		if strings.HasPrefix(p, pluginDir) {
			mode = 0o700
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			return "", err
		}
	}

	tfVersion, err := terraformVersion(ctx, tfPath)
	if err != nil {
		return "", err
	}

	env := append(os.Environ(),
		"TF_CLI_CONFIG_FILE="+filepath.Join(dir, "terraformrc"),
		"TF_IN_AUTOMATION=1",
		"TF_LOG=",
		"TF_PLUGIN_CACHE_DIR=",
		"CHECKPOINT_DISABLE=1",
	)

	runs := max(r.Opts.Runs, 1)
	timeout := r.Opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	var times []float64
	var out []byte
	for i := range runs {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		rctx, cancel := context.WithTimeout(ctx, timeout)
		cmd := exec.CommandContext(rctx, tfPath, "providers", "schema", "-json")
		cmd.Dir = dir
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		start := time.Now()
		rerr := cmd.Run()
		elapsed := time.Since(start)
		cancel()
		if rerr != nil {
			return "", fmt.Errorf("terraform providers schema: %w: %s", rerr, lastLines(stderr.String(), 6))
		}
		times = append(times, float64(elapsed.Microseconds())/1000)
		if i == 0 {
			out = stdout.Bytes()
		}
	}

	// provider RSS: one line per launch appended by the wrapper
	var maxRSS int64
	if b, err := os.ReadFile(rssFile); err == nil {
		for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
			if n, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64); err == nil && n > maxRSS {
				maxRSS = n
			}
		}
	}

	sr, err := parseSchema(out, r.P.Source)
	if err != nil {
		return "", err
	}
	sr.Platform = native
	sr.TerraformVersion = tfVersion
	sr.Runs = runs
	sr.WallMinMs = minOf(times)
	sr.WallMedMs = median(times)
	sr.ProviderMaxRSSBytes = maxRSS
	sr.JSONBytes = int64(len(out))
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(out)
	_ = gw.Close()
	sr.GzipBytes = int64(gz.Len())
	res.Schema = sr

	// keep the schema json for later digging, gzipped
	_ = os.WriteFile(filepath.Join(dir, "schema.json.gz"), gz.Bytes(), 0o600)

	return fmt.Sprintf("%.0fms (min %.0fms) rss %s json %s r=%d d=%d attrs=%d tf %s",
		sr.WallMedMs, sr.WallMinMs, humanBytes(sr.ProviderMaxRSSBytes), humanBytes(sr.JSONBytes), sr.Resources, sr.DataSources, sr.Attributes, tfVersion), nil
}

// ReparseSchema re-derives the schema counts of every stored result from the cached schema json, keeping the
// timings, so counts added to tfpp later do not need terraform re-run.
func (r *Runner) ReparseSchema(all []*results.Result) (int, error) {
	n := 0
	for _, res := range all {
		if res.Schema == nil {
			continue
		}
		gz, err := os.ReadFile(filepath.Join(r.P.SchemaDir(), res.Version, "schema.json.gz"))
		if err != nil {
			continue
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			return n, fmt.Errorf("%s: %w", res.Version, err)
		}
		raw, err := io.ReadAll(zr)
		if err != nil {
			return n, fmt.Errorf("%s: %w", res.Version, err)
		}
		sr, err := parseSchema(raw, r.P.Source)
		if err != nil {
			return n, fmt.Errorf("%s: %w", res.Version, err)
		}
		old := res.Schema
		sr.Platform, sr.TerraformVersion, sr.Runs = old.Platform, old.TerraformVersion, old.Runs
		sr.WallMinMs, sr.WallMedMs, sr.ProviderMaxRSSBytes = old.WallMinMs, old.WallMedMs, old.ProviderMaxRSSBytes
		sr.JSONBytes, sr.GzipBytes = old.JSONBytes, old.GzipBytes
		res.Schema = sr
		if err := r.Store.Save(res); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func terraformVersion(ctx context.Context, tf string) (string, error) {
	out, err := exec.CommandContext(ctx, tf, "version", "-json").Output()
	if err != nil {
		return "", fmt.Errorf("terraform version: %w", err)
	}
	var v struct {
		TerraformVersion string `json:"terraform_version"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return "", fmt.Errorf("parsing terraform version: %w", err)
	}
	return v.TerraformVersion, nil
}

type schemaJSON struct {
	ProviderSchemas map[string]struct {
		Provider      schemaBlockWrap            `json:"provider"`
		Resources     map[string]schemaBlockWrap `json:"resource_schemas"`
		DataSources   map[string]schemaBlockWrap `json:"data_source_schemas"`
		Ephemeral     map[string]schemaBlockWrap `json:"ephemeral_resource_schemas"`
		ListResources map[string]schemaBlockWrap `json:"list_resource_schemas"`
		Actions       map[string]json.RawMessage `json:"action_schemas"`
		Functions     map[string]json.RawMessage `json:"functions"`
		Identities    map[string]json.RawMessage `json:"resource_identity_schemas"`
	} `json:"provider_schemas"`
}

type schemaBlockWrap struct {
	Block schemaBlock `json:"block"`
}

type schemaBlock struct {
	Attributes map[string]struct {
		Deprecated bool `json:"deprecated"`
	} `json:"attributes"`
	BlockTypes map[string]struct {
		Block schemaBlock `json:"block"`
	} `json:"block_types"`
}

func (b schemaBlock) walk(depth int, sr *results.SchemaResult) {
	if depth > sr.MaxDepth {
		sr.MaxDepth = depth
	}
	sr.Attributes += len(b.Attributes)
	for _, a := range b.Attributes {
		if a.Deprecated {
			sr.DeprecatedAttrs++
		}
	}
	sr.Blocks += len(b.BlockTypes)
	for _, bt := range b.BlockTypes {
		bt.Block.walk(depth+1, sr)
	}
}

func parseSchema(out []byte, source string) (*results.SchemaResult, error) {
	var sj schemaJSON
	if err := json.Unmarshal(out, &sj); err != nil {
		return nil, fmt.Errorf("parsing schema json: %w", err)
	}
	sr := &results.SchemaResult{}
	found := false
	for addr, ps := range sj.ProviderSchemas {
		if !strings.HasSuffix(addr, "/"+source) {
			continue
		}
		found = true
		sr.Resources = len(ps.Resources)
		sr.DataSources = len(ps.DataSources)
		sr.EphemeralResources = len(ps.Ephemeral)
		sr.ListResources = len(ps.ListResources)
		sr.Actions = len(ps.Actions)
		sr.Functions = len(ps.Functions)
		sr.IdentityResources = len(ps.Identities)
		sr.ProviderAttributes = len(ps.Provider.Block.Attributes)
		for _, r := range ps.Resources {
			r.Block.walk(1, sr)
		}
		for _, d := range ps.DataSources {
			d.Block.walk(1, sr)
		}
	}
	if !found {
		return nil, fmt.Errorf("provider %s not present in schema output", source)
	}
	return sr, nil
}

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/katbyte/tf-provider-profile/lib/clog"
	"github.com/katbyte/tf-provider-profile/lib/provider"
	"github.com/katbyte/tf-provider-profile/lib/results"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type FlagData struct {
	Provider   string `mapstructure:"provider"`
	Repo       string `mapstructure:"repo"`
	CacheDir   string `mapstructure:"cache-dir"`
	DataDir    string `mapstructure:"data-dir"`
	ReportsDir string `mapstructure:"reports-dir"`

	Since     time.Time `mapstructure:"-"`
	Versions  []string  `mapstructure:"versions"`
	Platforms []string  `mapstructure:"platforms"`
	Stages    []string  `mapstructure:"stages"`
	Sample    int       `mapstructure:"sample"`

	Force        bool          `mapstructure:"force"`
	Retry        bool          `mapstructure:"retry"`
	Concurrency  int           `mapstructure:"concurrency"`
	Runs         int           `mapstructure:"runs"`
	Terraform    string        `mapstructure:"terraform"`
	GolangciLint string        `mapstructure:"golangci-lint"`
	Timeout      time.Duration `mapstructure:"timeout"`
	FullClone    bool          `mapstructure:"full-clone"`
	NoReport     bool          `mapstructure:"no-report"`

	GitHubToken  string `mapstructure:"token-gh"`
	GitHubAPIURL string `mapstructure:"github-api-url"`
}

func configureFlags(root *cobra.Command) error {
	pflags := root.PersistentFlags()

	pflags.StringP("provider", "p", "azurerm", "provider short name (hashicorp/terraform-provider-<name>)")
	pflags.String("repo", "", "github repo of the provider, if not hashicorp/terraform-provider-<provider>")
	pflags.String("cache-dir", ".cache", "root directory for everything regenerable: downloads, source, build caches (<cache-dir>/<provider>/)")
	pflags.String("data-dir", "data", "root directory for the collected per-release results, one json per release, meant to be committed (<data-dir>/<provider>/)")
	pflags.String("reports-dir", "reports", "root directory for rendered reports (<reports-dir>/<provider>/)")

	pflags.String("since", "2024-07-01", "only releases published on or after this date (YYYY-MM-DD)")
	pflags.StringSlice("versions", nil, "only these release tags (comma separated, e.g. v5.3.0,v5.4.0); overrides --since")
	pflags.StringSlice("platforms", []string{"linux_amd64", provider.NativePlatform()}, "release platforms to download and dissect; timing stages always use the native one")
	pflags.StringSlice("stages", results.AllStages, "stages to run: "+strings.Join(results.AllStages, ","))
	pflags.IntP("sample", "n", 1, "run the expensive stages ("+strings.Join(results.ExpensiveStages, ",")+") for every nth release only (newest always included)")

	pflags.BoolP("force", "f", false, "rerun stages that already completed")
	pflags.Bool("retry", false, "rerun stages that previously failed")
	pflags.Int("concurrency", 4, "parallel downloads")
	pflags.Int("runs", 7, "repetitions for the startup and schema timing stages (min and median are recorded)")
	pflags.String("terraform", "terraform", "terraform binary used by the schema stage")
	pflags.String("golangci-lint", "golangci-lint", "golangci-lint binary used by the lint stage when the provider has no custom one")
	pflags.Duration("timeout", 0, "per-command timeout for timing stages (0 = stage default)")
	pflags.Bool("full-clone", false, "clone the full repository instead of a blobless partial clone")
	pflags.Bool("no-report", false, "do not regenerate the report after a run")

	pflags.String("token-gh", "", "github token for the releases api (consider exporting GITHUB_TOKEN instead)")
	pflags.String("github-api-url", "", "override the GitHub API base URL")
	if err := pflags.MarkHidden("github-api-url"); err != nil {
		return err
	}

	pflags.BoolP("verbose", "v", false, "show detailed output")
	pflags.Bool("quiet", false, "minimal output")
	pflags.Bool("silent", false, "suppress all output")

	// binding map for viper/pflag -> env
	m := map[string]string{ //nolint:gosec // G101: these are env var names, not credentials
		"provider":       "TFPP_PROVIDER",
		"repo":           "TFPP_REPO",
		"cache-dir":      "TFPP_CACHE_DIR",
		"reports-dir":    "TFPP_REPORTS_DIR",
		"data-dir":       "TFPP_DATA_DIR",
		"since":          "TFPP_SINCE",
		"versions":       "",
		"platforms":      "TFPP_PLATFORMS",
		"stages":         "",
		"sample":         "TFPP_SAMPLE",
		"force":          "",
		"retry":          "",
		"concurrency":    "TFPP_CONCURRENCY",
		"runs":           "TFPP_RUNS",
		"terraform":      "TFPP_TERRAFORM",
		"golangci-lint":  "TFPP_GOLANGCI_LINT",
		"timeout":        "TFPP_TIMEOUT",
		"full-clone":     "TFPP_FULL_CLONE",
		"no-report":      "",
		"token-gh":       "GITHUB_TOKEN",
		"github-api-url": "TFPP_GITHUB_API_URL",
		"verbose":        "",
		"quiet":          "TFPP_OUTPUT_QUIET",
		"silent":         "TFPP_OUTPUT_SILENT",
	}

	for name, env := range m {
		if err := viper.BindPFlag(name, pflags.Lookup(name)); err != nil {
			return fmt.Errorf("error binding '%s' flag: %w", name, err)
		}
		if env != "" {
			if err := viper.BindEnv(name, env); err != nil {
				return fmt.Errorf("error binding '%s' to env '%s' : %w", name, env, err)
			}
		}
	}

	viper.SetConfigName(".tfpp")
	viper.SetConfigType("env")
	if home, err := os.UserHomeDir(); err == nil {
		viper.AddConfigPath(home)
	}
	viper.AddConfigPath(".")

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := errors.AsType[viper.ConfigFileNotFoundError](err); !ok {
			clog.Log.Errorf("Error reading config file: %v", err)
		}
	}

	return nil
}

// GetFlags returns the fully populated FlagData, unmarshalled from viper so env/config overrides apply.
func GetFlags() *FlagData {
	var f FlagData
	if err := viper.Unmarshal(&f); err != nil {
		clog.Log.Fatalf("failed to unmarshal configuration: %v", err)
	}

	since, err := time.Parse("2006-01-02", viper.GetString("since"))
	if err != nil {
		clog.Log.Fatalf("--since must be YYYY-MM-DD: %v", err)
	}
	f.Since = since.UTC()

	f.Platforms = dedupe(f.Platforms)
	f.Stages = dedupe(f.Stages)
	if f.Sample < 1 {
		f.Sample = 1
	}
	return &f
}

func dedupe(in []string) []string {
	var out []string
	for _, s := range in {
		for part := range strings.SplitSeq(s, ",") {
			part = strings.TrimSpace(part)
			if part != "" && !slices.Contains(out, part) {
				out = append(out, part)
			}
		}
	}
	return out
}

func (f *FlagData) provider() (*provider.Provider, error) {
	return provider.New(f.Provider, f.Repo, f.CacheDir, f.DataDir)
}

// selectReleases resolves the provider and the releases selected by the flags, returning also every known release.
func (f *FlagData) selectReleases(ctx context.Context) (p *provider.Provider, selected, all []provider.Release, err error) {
	p, err = f.provider()
	if err != nil {
		return nil, nil, nil, err
	}

	since := f.Since
	if len(f.Versions) > 0 {
		since = time.Time{} // explicit versions may predate --since
	}
	rels, err := p.FetchReleases(ctx, f.GitHubAPIURL, f.GitHubToken, since)
	if err != nil {
		return nil, nil, nil, err
	}
	all, err = p.LoadCachedReleases()
	if err != nil {
		all = rels
	}

	if len(f.Versions) > 0 {
		want := dedupe(f.Versions)
		var sel []provider.Release
		for _, r := range rels {
			if slices.Contains(want, r.Version) || slices.Contains(want, strings.TrimPrefix(r.Version, "v")) {
				sel = append(sel, r)
			}
		}
		if len(sel) != len(want) {
			return nil, nil, nil, fmt.Errorf("%d of %d requested versions not found in releases: got %v", len(want)-len(sel), len(want), sel)
		}
		rels = sel
	}
	return p, rels, all, nil
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

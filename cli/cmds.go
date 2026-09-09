// Package cli implements the tfpp command line interface: the cobra commands, flag and config handling, and the
// orchestration of profiling runs and report generation.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/katbyte/tf-provider-profile/lib/cout"
	"github.com/katbyte/tf-provider-profile/lib/provider"
	"github.com/katbyte/tf-provider-profile/lib/report"
	"github.com/katbyte/tf-provider-profile/lib/results"
	"github.com/katbyte/tf-provider-profile/lib/stages"
	"github.com/katbyte/tf-provider-profile/lib/version"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func Make() (*cobra.Command, error) {
	root := &cobra.Command{
		Use:   "tfpp [command]",
		Short: "tfpp profiles terraform provider releases over time",
		Long: `tfpp downloads every release of a terraform provider and records how it has changed over time: compressed
and uncompressed binary size and what the binary is made of, plugin startup and schema fetch cost, source tree
metrics at each tag, and (sampled) clean build and lint times. Results are cached under .cache/<provider>/ and
rendered into an interactive html report under reports/<provider>/.

Complete documentation is available at https://github.com/katbyte/tf-provider-profile`,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			switch {
			case viper.GetBool("silent"):
				cout.Level = cout.VerbositySilent
			case viper.GetBool("quiet"):
				cout.Level = cout.VerbosityQuiet
			case viper.GetBool("verbose"):
				cout.Level = cout.VerbosityVerbose
			}
			return nil
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Println("Run \"tfpp help\" for more information about available tfpp commands.")
			return nil
		},
	}

	root.AddCommand(&cobra.Command{
		Use:           "version",
		Short:         "Print the version number of tfpp",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println("tfpp " + version.Version)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:           "releases",
		Short:         "list the releases selected by --since/--versions and which stages have run for each",
		Aliases:       []string{"ls"},
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			f := GetFlags()
			ps, err := f.providers()
			if err != nil {
				return err
			}
			for i, p := range ps {
				if i > 0 {
					cout.Println()
				}
				if err := listReleases(cmd.Context(), f, p); err != nil {
					return err
				}
			}
			return nil
		},
	})

	root.AddCommand(&cobra.Command{
		Use:           "clone",
		Short:         "clone (or update) the provider source into .cache/<provider>/src",
		Long:          "Clones the provider repository for the source, build and lint stages. Uses a blobless partial clone by default (history without file contents, fetched on checkout) to keep the cache small; --full-clone fetches everything.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			f := GetFlags()
			ps, err := f.providers()
			if err != nil {
				return err
			}
			for _, p := range ps {
				if err := cloneSource(cmd.Context(), p, f.FullClone); err != nil {
					return err
				}
			}
			return nil
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "run",
		Short: "download releases and run the profiling stages",
		Long: `Runs the selected stages against every release matching --since/--versions, for --provider or for every
provider listed in .tfpp.yml. Completed stages are skipped on rerun (use --force to redo them, --retry to redo failed
ones), so the command is safe to run repeatedly as new releases appear. The expensive stages (` + strings.Join(results.ExpensiveStages, ", ") + `) only run for every --sample'th release.

Stages: ` + strings.Join(results.AllStages, ", "),
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			f := GetFlags()
			for _, s := range f.Stages {
				if !slices.Contains(results.AllStages, s) {
					return fmt.Errorf("unknown stage %q (valid: %s)", s, strings.Join(results.AllStages, ", "))
				}
			}
			ps, err := f.providers()
			if err != nil {
				return err
			}
			for i, p := range ps {
				if i > 0 {
					cout.Println()
				}
				if err := runProvider(cmd.Context(), f, p); err != nil {
					return fmt.Errorf("%s: %w", p.Name, err)
				}
			}
			if f.NoReport {
				return nil
			}
			return writeReport(f)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:           "report",
		Short:         "render reports/ (index.html per provider and combined, data.json, data.csv) from the collected results",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return writeReport(GetFlags())
		},
	})

	root.AddCommand(&cobra.Command{
		Use:           "reparse",
		Short:         "re-derive schema counts from the cached schema json without re-running terraform",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			f := GetFlags()
			ps, err := f.providers()
			if err != nil {
				return err
			}
			for _, p := range ps {
				store := results.NewStore(p)
				all, err := store.LoadAll()
				if err != nil {
					return err
				}
				n, err := (&stages.Runner{P: p, Store: store}).ReparseSchema(all)
				if err != nil {
					return err
				}
				cout.Printf("<white>==></> re-parsed schema counts for <yellow>%d</> <cyan>%s</> releases\n", n, p.Name)
			}
			return writeReport(f)
		},
	})

	wrap := &cobra.Command{
		Use:    "_wrap --rss-out FILE -- BINARY [ARGS...]",
		Short:  "internal: exec a provider on terraform's behalf and record its peak rss",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			out, _ := cmd.Flags().GetString("rss-out")
			os.Exit(stages.Wrap(out, args)) //nolint:revive // deep-exit: the wrapper must propagate the provider's exit code to terraform
		},
	}
	wrap.Flags().String("rss-out", "", "file to append the child's peak rss (bytes) to")
	root.AddCommand(wrap)

	if err := configureFlags(root); err != nil {
		return nil, fmt.Errorf("unable to configure flags: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	root.SetContext(ctx)
	cobra.OnFinalize(cancel)

	return root, nil
}

// listReleases prints the release table for one provider.
func listReleases(ctx context.Context, f *FlagData, p *provider.Provider) error {
	rels, all, err := f.selectReleases(ctx, p)
	if err != nil {
		return err
	}
	sampled := map[string]bool{}
	for _, r := range provider.Sample(rels, f.Sample) {
		sampled[r.Version] = true
	}
	store := results.NewStore(p)

	cout.Printf("<white>%d</> releases of <cyan>%s</> since %s (%d known)\n\n", len(rels), p.Name, f.Since.Format("2006-01-02"), len(all))
	cout.Printf("%-10s %-10s %-3s ", "version", "date", "smp")
	for _, s := range results.AllStages {
		cout.Printf("%-9s", s)
	}
	cout.Println()
	for _, r := range rels {
		res, err := store.Load(r)
		if err != nil {
			return err
		}
		s := " "
		if sampled[r.Version] {
			s = "*"
		}
		cout.Printf("%-10s %-10s  %s  ", r.Version, r.Date.Format("2006-01-02"), s)
		for _, st := range results.AllStages {
			switch {
			case st == results.StageDownload:
				cout.Printf("%-9s", downloadStatus(p, f.Platforms, r.Version))
			case res.Done(st):
				cout.Printf("<green>%-9s</>", "ok")
			case res.Failed(st):
				cout.Printf("<red>%-9s</>", "failed")
			default:
				cout.Printf("<gray>%-9s</>", "-")
			}
		}
		cout.Println()
	}
	cout.Printf("\n<gray>* = selected by --sample %d for the expensive stages (%s)</>\n", f.Sample, strings.Join(results.ExpensiveStages, ", "))
	return nil
}

// runProvider downloads and profiles the selected releases of one provider.
func runProvider(ctx context.Context, f *FlagData, p *provider.Provider) error {
	rels, all, err := f.selectReleases(ctx, p)
	if err != nil {
		return err
	}
	if len(rels) == 0 {
		return errors.New("no releases selected")
	}

	needsSrc := slices.ContainsFunc(f.Stages, func(s string) bool {
		return s == results.StageSource || s == results.StageBuild || s == results.StageLint
	})
	if needsSrc {
		if _, err := os.Stat(p.SrcDir()); err != nil {
			if err := cloneSource(ctx, p, f.FullClone); err != nil {
				return err
			}
		}
	}

	sampled := provider.Sample(rels, f.Sample)
	cout.Printf("profiling <cyan>%s</>: <yellow>%d</> releases (%s .. %s), stages %s, sampling every %d for %s (%d releases)\n",
		p.Name, len(rels), rels[0].Version, rels[len(rels)-1].Version, strings.Join(f.Stages, ","), f.Sample, strings.Join(results.ExpensiveStages, ","), len(sampled))

	runner := &stages.Runner{
		P:     p,
		Store: results.NewStore(p),
		All:   all,
		Opts: stages.Options{
			Platforms:    f.Platforms,
			Stages:       f.Stages,
			Force:        f.Force,
			Retry:        f.Retry,
			Concurrency:  f.Concurrency,
			Runs:         f.Runs,
			Terraform:    f.Terraform,
			GolangciLint: f.GolangciLint,
			Timeout:      f.Timeout,
		},
	}
	return runner.Run(ctx, rels, sampled)
}

func downloadStatus(p *provider.Provider, platforms []string, ver string) string {
	n := 0
	for _, pl := range platforms {
		if _, err := os.Stat(p.ReleaseDir(ver) + "/" + p.ZipName(ver, pl)); err == nil {
			n++
		}
	}
	switch {
	case n == len(platforms):
		return "ok"
	case n == 0:
		return "-"
	}
	return fmt.Sprintf("%d/%d", n, len(platforms))
}

func cloneSource(ctx context.Context, p *provider.Provider, full bool) error {
	src := p.SrcDir()
	if _, err := os.Stat(src + "/.git"); err == nil {
		cout.Printf("<white>==></> updating source clone in %s\n", src)
		return runGit(ctx, src, "fetch", "--quiet", "--tags", "origin")
	}
	if err := os.MkdirAll(p.CacheDir, 0o750); err != nil {
		return err
	}
	args := []string{"clone", "--quiet"}
	if !full {
		args = append(args, "--filter=blob:none")
	}
	args = append(args, "https://github.com/"+p.Repo+".git", src)
	cout.Printf("<white>==></> cloning %s into %s (this takes a while)\n", p.Repo, src)
	start := time.Now()
	if err := runGit(ctx, "", args...); err != nil {
		return err
	}
	cout.Printf("    done in %.0fs\n", time.Since(start).Seconds())
	return nil
}

// writeReport renders every provider listed in .tfpp.yml plus any other with results under the data dir, and the
// combined page, so the site always reflects all collected data.
func writeReport(f *FlagData) error {
	var ps []*provider.Provider
	seen := map[string]bool{}
	for _, s := range f.Providers {
		p, err := provider.New(s.Name, s.Repo, f.CacheDir, f.DataDir)
		if err != nil {
			return err
		}
		ps = append(ps, p)
		seen[s.Name] = true
	}
	if entries, err := os.ReadDir(f.DataDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() || seen[e.Name()] {
				continue
			}
			p, err := provider.New(e.Name(), "", f.CacheDir, f.DataDir)
			if err != nil {
				return err
			}
			ps = append(ps, p)
		}
	}

	var pds []report.ProviderData
	total := 0
	for _, p := range ps {
		all, err := results.NewStore(p).LoadAll()
		if err != nil {
			return err
		}
		if len(all) == 0 {
			continue
		}
		total += len(all)
		pds = append(pds, report.ProviderData{P: p, All: all})
	}
	if err := report.WriteAll(pds, f.ReportsDir); err != nil {
		return err
	}
	cout.Printf("\n<white>==></> report written to <cyan>%s/index.html</> (%d providers, %d releases)\n", f.ReportsDir, len(pds), total)
	return nil
}

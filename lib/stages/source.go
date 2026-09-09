package stages

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/katbyte/tf-provider-profile/lib/results"
)

var (
	reTypedResource   = regexp.MustCompile(`(?m)^var _ sdk\.Resource(With[A-Za-z]+)? = `)
	reTypedDataSource = regexp.MustCompile(`(?m)^var _ sdk\.DataSource(With[A-Za-z]+)? = `)
	reUntypedResource = regexp.MustCompile(`(?m)^func resource[A-Za-z0-9]*\(\) \*(pluginsdk|schema)\.Resource \{`)
	reUntypedDataSrc  = regexp.MustCompile(`(?m)^func dataSource[A-Za-z0-9]*\(\) \*(pluginsdk|schema)\.Resource \{`)
	reTestFunc        = regexp.MustCompile(`(?m)^func Test[A-Za-z0-9_]*\(`)
	reAccTestFunc     = regexp.MustCompile(`(?m)^func TestAcc[A-Za-z0-9_]*\(`)
	reTypedList       = regexp.MustCompile(`(?m)^var _ sdk\.FrameworkListWrappedResource(With[A-Za-z]+)? = `)
	reTypedAction     = regexp.MustCompile(`(?m)^var _ sdk\.Action(With[A-Za-z]+)? = `)
	reTypedEphemeral  = regexp.MustCompile(`(?m)^var _ sdk\.EphemeralResource(With[A-Za-z]+)? = `)
	reIdentity        = regexp.MustCompile(`Identity:\s*&(schema|pluginsdk)\.ResourceIdentity\{|(?m)^var _ sdk\.ResourceWithIdentity = `)
	reImportLegacySDK = regexp.MustCompile(`"github\.com/Azure/azure-sdk-for-go/`)
	reImportKermit    = regexp.MustCompile(`"github\.com/[a-z0-9-]+/kermit/`)
	reImportAutorest  = regexp.MustCompile(`"github\.com/Azure/go-autorest/`)
	reImportGoAzure   = regexp.MustCompile(`"github\.com/hashicorp/go-azure-sdk/`)
	reShortStat       = regexp.MustCompile(`(\d+) files? changed(?:, (\d+) insertions?\(\+\))?(?:, (\d+) deletions?\(-\))?`)
	reChangelogHead   = regexp.MustCompile(`^## v?(\S+)`)
	reChangelogSect   = regexp.MustCompile(`^([A-Z][A-Z /&]+):\s*$`)
)

// checkoutTag checks the source clone out at the release tag, fetching if the tag is unknown.
func (r *Runner) checkoutTag(ctx context.Context, version string) (string, error) {
	src := r.P.SrcDir()
	if _, err := os.Stat(filepath.Join(src, ".git")); err != nil {
		return "", fmt.Errorf("source clone missing at %s (run `tfpp clone`): %w", src, err)
	}
	if _, err := runCmd(ctx, src, nil, "git", "rev-parse", "--verify", "--quiet", version+"^{commit}"); err != nil {
		if _, err := runCmd(ctx, src, nil, "git", "fetch", "--quiet", "--tags", "origin"); err != nil {
			return "", err
		}
	}
	if _, err := runCmd(ctx, src, nil, "git", "checkout", "--quiet", "--force", version); err != nil {
		return "", err
	}
	out, err := runCmd(ctx, src, nil, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// sourceStage counts what is in the tree at the release tag plus what changed since the previous release.
func (r *Runner) sourceStage(ctx context.Context, res *results.Result) (string, error) {
	commit, err := r.checkoutTag(ctx, res.Version)
	if err != nil {
		return "", err
	}
	src := r.P.SrcDir()
	sr := &results.SourceResult{Commit: commit, ChangelogEntries: map[string]int{}}

	if err := walkTree(src, sr); err != nil {
		return "", err
	}
	if err := parseGoMod(src, sr); err != nil {
		return "", err
	}
	countDocs(src, sr)
	sr.Services = countDirs(filepath.Join(src, "internal", "services"))
	if sr.Services == 0 {
		sr.Services = countDirs(filepath.Join(src, "internal", "service")) // aws layout
	}
	if b, err := os.ReadFile(filepath.Join(src, ".go-version")); err == nil {
		sr.GoVersionFile = strings.TrimSpace(string(b))
	}
	parseChangelog(src, res.Version, sr)

	if prev, ok := r.prevRelease(res.Version); ok {
		sr.PrevVersion = prev.Version
		sr.DaysSincePrev = res.Date.Sub(prev.Date).Hours() / 24
		if err := r.gitStats(ctx, prev.Version, res.Version, sr); err != nil {
			return "", err
		}
	}

	res.Source = sr
	return fmt.Sprintf("go %s loc (%s test) vendor %s svcs %d typed %d/%d untyped %d/%d docs r=%d d=%d commits %d",
		humanCount(sr.GoLines), humanCount(sr.GoTestLines), humanCount(sr.VendorGoLines), sr.Services,
		sr.TypedResources, sr.TypedDataSources, sr.UntypedResources, sr.UntypedDataSources,
		sr.DocsResources, sr.DocsDataSources, sr.Commits), nil
}

func humanCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return strconv.Itoa(n)
}

func countDirs(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	return n
}

func countFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			n++
		}
	}
	return n
}

func countDocs(src string, sr *results.SourceResult) {
	docs := filepath.Join(src, "website", "docs")
	sr.DocsResources = countFiles(filepath.Join(docs, "r"))
	sr.DocsDataSources = countFiles(filepath.Join(docs, "d"))
	sr.DocsEphemeral = countFiles(filepath.Join(docs, "ephemeral-resources")) + countFiles(filepath.Join(docs, "e"))
	sr.DocsListResources = countFiles(filepath.Join(docs, "list-resources"))
	sr.DocsActions = countFiles(filepath.Join(docs, "actions"))
	sr.DocsFunctions = countFiles(filepath.Join(docs, "functions"))
	sr.DocsGuides = countFiles(filepath.Join(docs, "guides"))
}

// walkTree counts files and go lines, separating vendor and tests, and greps the service packages for resource
// registration patterns.
func walkTree(src string, sr *results.SourceResult) error {
	typedRes, typedDS := map[string]bool{}, map[string]bool{}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if d.IsDir() {
			if rel == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // vanished mid-walk; skip it
		}
		sr.TotalFiles++
		sr.TreeBytes += info.Size()
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		vendor := strings.HasPrefix(rel, "vendor"+string(filepath.Separator))
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines, code := countLines(b)
		if vendor {
			sr.VendorGoFiles++
			sr.VendorGoLines += lines
			return nil
		}

		sr.GoFiles++
		sr.GoLines += lines
		sr.GoCodeLines += code
		if strings.HasSuffix(path, "_test.go") {
			sr.GoTestFiles++
			sr.GoTestLines += lines
			sr.TestFuncs += len(reTestFunc.FindAllIndex(b, -1))
			sr.AccTestFuncs += len(reAccTestFunc.FindAllIndex(b, -1))
			return nil
		}

		if strings.HasPrefix(rel, filepath.Join("internal", "services")) || strings.HasPrefix(rel, filepath.Join("internal", "service")) {
			if reTypedResource.Match(b) {
				typedRes[path] = true
			}
			if reTypedDataSource.Match(b) {
				typedDS[path] = true
			}
			untyped := len(reUntypedResource.FindAllIndex(b, -1)) + len(reUntypedDataSrc.FindAllIndex(b, -1))
			sr.UntypedResources += len(reUntypedResource.FindAllIndex(b, -1))
			sr.UntypedDataSources += len(reUntypedDataSrc.FindAllIndex(b, -1))
			legacy := reImportLegacySDK.Match(b) || reImportKermit.Match(b)
			modern := reImportGoAzure.Match(b)
			class := "none"
			switch {
			case legacy && modern:
				class = "both"
			case legacy:
				class = "legacy"
			case modern:
				class = "go_azure_sdk"
			}
			if untyped > 0 || typedRes[path] || typedDS[path] {
				switch class {
				case "both":
					sr.ResourceFilesBothSDK++
				case "legacy":
					sr.ResourceFilesLegacySDK++
				case "go_azure_sdk":
					sr.ResourceFilesGoAzureSDK++
				default:
					sr.ResourceFilesNoSDK++
				}
			}
			kinds := map[string]bool{
				"resource":    len(reUntypedResource.FindAllIndex(b, -1)) > 0 || typedRes[path],
				"data_source": len(reUntypedDataSrc.FindAllIndex(b, -1)) > 0 || typedDS[path],
				"list":        reTypedList.Match(b),
				"action":      reTypedAction.Match(b),
				"ephemeral":   reTypedEphemeral.Match(b),
			}
			for kind, ok := range kinds {
				if !ok {
					continue
				}
				if sr.SDKByKind == nil {
					sr.SDKByKind = map[string]map[string]int{}
				}
				if sr.SDKByKind[kind] == nil {
					sr.SDKByKind[kind] = map[string]int{}
				}
				sr.SDKByKind[kind][class]++
			}
			if kinds["resource"] && reIdentity.Match(b) {
				sr.IdentityResourceFiles++
			}
			if reImportLegacySDK.Match(b) {
				sr.FilesImportingLegacySDK++
			}
			if reImportKermit.Match(b) {
				sr.FilesImportingKermit++
			}
			if reImportAutorest.Match(b) {
				sr.FilesImportingAutorest++
			}
			if reImportGoAzure.Match(b) {
				sr.FilesImportingGoAzureSDK++
			}
		}
		sr.TypedResources = len(typedRes)
		sr.TypedDataSources = len(typedDS)
		return nil
	})
}

func countLines(b []byte) (total, code int) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		total++
		t := bytes.TrimSpace(sc.Bytes())
		if len(t) > 0 && !bytes.HasPrefix(t, []byte("//")) {
			code++
		}
	}
	return total, code
}

func parseGoMod(src string, sr *results.SourceResult) error {
	b, err := os.ReadFile(filepath.Join(src, "go.mod"))
	if err != nil {
		return fmt.Errorf("reading go.mod: %w", err)
	}
	inRequire := false
	for line := range strings.SplitSeq(string(b), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "go "):
			sr.GoDirective = strings.TrimSpace(strings.TrimPrefix(t, "go "))
		case strings.HasPrefix(t, "require ("):
			inRequire = true
		case inRequire && t == ")":
			inRequire = false
		case inRequire && t != "" && !strings.HasPrefix(t, "//"):
			if strings.Contains(t, "// indirect") {
				sr.IndirectDeps++
			} else {
				sr.DirectDeps++
			}
		case strings.HasPrefix(t, "require ") && !strings.Contains(t, "("):
			if strings.Contains(t, "// indirect") {
				sr.IndirectDeps++
			} else {
				sr.DirectDeps++
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(src, "go.sum")); err == nil {
		sr.GoSumLines = bytes.Count(b, []byte("\n"))
	}
	return nil
}

// parseChangelog counts bullets per section under the release's heading in CHANGELOG.md (or an archived
// CHANGELOG-vN.md).
func parseChangelog(src, version string, sr *results.SourceResult) {
	want := strings.TrimPrefix(version, "v")
	files, _ := filepath.Glob(filepath.Join(src, "CHANGELOG*.md"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		in := false
		section := ""
		for line := range strings.SplitSeq(string(b), "\n") {
			if m := reChangelogHead.FindStringSubmatch(line); m != nil {
				if in {
					return
				}
				in = m[1] == want
				continue
			}
			if !in {
				continue
			}
			if m := reChangelogSect.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				section = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(m[1]), " ", "_"))
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(line), "* ") && section != "" {
				sr.ChangelogEntries[section]++
				sr.ChangelogEntries["total"]++
			}
		}
		if in {
			return
		}
	}
}

func (r *Runner) gitStats(ctx context.Context, prev, cur string, sr *results.SourceResult) error {
	src := r.P.SrcDir()
	rng := prev + ".." + cur

	out, err := runCmd(ctx, src, nil, "git", "rev-list", "--count", rng)
	if err != nil {
		return err
	}
	sr.Commits, _ = strconv.Atoi(strings.TrimSpace(out))

	out, err = runCmd(ctx, src, nil, "git", "log", "--format=%aN", rng)
	if err != nil {
		return err
	}
	authors := map[string]bool{}
	for a := range strings.SplitSeq(out, "\n") {
		if a = strings.TrimSpace(a); a != "" {
			authors[a] = true
		}
	}
	sr.Authors = len(authors)

	// exclude vendor and the changelog so the numbers describe provider changes rather than dependency churn
	out, err = runCmd(ctx, src, nil, "git", "diff", "--shortstat", prev, cur, "--", ".", ":(exclude)vendor", ":(exclude)CHANGELOG.md")
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return err
		}
		return err
	}
	if m := reShortStat.FindStringSubmatch(out); m != nil {
		sr.FilesChanged, _ = strconv.Atoi(m[1])
		sr.LinesAdded, _ = strconv.Atoi(m[2])
		sr.LinesRemoved, _ = strconv.Atoi(m[3])
	}
	return nil
}

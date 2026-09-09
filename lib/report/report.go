package report

import (
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/katbyte/tf-provider-profile/lib/provider"
	"github.com/katbyte/tf-provider-profile/lib/results"
	"github.com/katbyte/tf-provider-profile/lib/version"
)

//go:embed template.html
var templateHTML string

type releaseInfo struct {
	Version   string            `json:"version"`
	Date      string            `json:"date"`
	TS        int64             `json:"ts"`
	GoVersion string            `json:"go_version,omitempty"`
	Terraform string            `json:"terraform,omitempty"`
	Toolchain string            `json:"toolchain,omitempty"`
	PluginSDK string            `json:"plugin_sdk,omitempty"` // terraform-plugin-sdk/v2 version linked
	Framework string            `json:"framework,omitempty"`  // terraform-plugin-framework version linked
	Host      string            `json:"host,omitempty"`       // machine the timing stages ran on
	DepsAdded []string          `json:"deps_added,omitempty"` // modules linked that the previous release did not have
	DepsGone  []string          `json:"deps_removed,omitempty"`
	Stages    map[string]string `json:"stages"` // stage -> ok | failed: <err>
}

type data struct {
	Provider    string                            `json:"provider"`
	Repo        string                            `json:"repo"`
	Generated   string                            `json:"generated"`
	Tool        string                            `json:"tool"`
	Native      string                            `json:"native"`
	Platforms   []string                          `json:"platforms"`
	Releases    []releaseInfo                     `json:"releases"`
	Groups      []Group                           `json:"groups"`
	Metrics     []Metric                          `json:"metrics"`
	Composition map[string]map[string]Composition `json:"composition"` // platform -> modules|services|sections
}

const compModules = "modules"

// ProviderData is one provider's stored results.
type ProviderData struct {
	P   *provider.Provider
	All []*results.Result
}

// WriteAll renders reports/<provider>/ for every provider and a combined reports/index.html that embeds all of them.
func WriteAll(pds []ProviderData, reportsRoot string) error {
	if err := os.MkdirAll(reportsRoot, 0o750); err != nil {
		return err
	}
	var all []data
	for _, pd := range pds {
		if len(pd.All) == 0 {
			continue
		}
		d := build(pd.P, pd.All)
		all = append(all, d)
		dir := filepath.Join(reportsRoot, pd.P.Name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
		js, err := json.Marshal(d)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "data.json"), js, 0o600); err != nil {
			return err
		}
		if err := writeCSV(filepath.Join(dir, "data.csv"), d); err != nil {
			return err
		}
		if err := writePage(filepath.Join(dir, "index.html"), []data{d}, pd.P.Name); err != nil {
			return err
		}
	}
	if len(all) == 0 {
		return errors.New("no results to report; run `tfpp run` first")
	}
	return writePage(filepath.Join(reportsRoot, "index.html"), all, "all")
}

// writePage embeds the datasets in the template so the page works from file:// with no server.
func writePage(path string, ds []data, title string) error {
	js, err := json.Marshal(map[string]any{"providers": ds})
	if err != nil {
		return err
	}
	// "</" must not terminate the script tag
	safe := strings.ReplaceAll(string(js), "</", "<\\/")
	html := strings.Replace(templateHTML, "/*__TFPP_DATA__*/", safe, 1)
	html = strings.ReplaceAll(html, "__TFPP_PROVIDER__", title)
	return os.WriteFile(path, []byte(html), 0o600)
}

func build(p *provider.Provider, all []*results.Result) data {
	all = dropBackports(all)
	native := provider.NativePlatform()
	plats := platforms(all, native)
	d := data{
		Provider:    p.Name,
		Repo:        p.Repo,
		Generated:   time.Now().UTC().Format(time.RFC3339),
		Tool:        "tfpp " + version.Version,
		Native:      native,
		Platforms:   plats,
		Groups:      groups,
		Metrics:     buildMetrics(all, plats),
		Composition: map[string]map[string]Composition{},
	}

	var prevDeps map[string]string
	for _, r := range all {
		ri := releaseInfo{Version: r.Version, Date: r.Date.Format("2006-01-02"), TS: r.Date.Unix(), Stages: map[string]string{}}
		for _, pl := range plats {
			b := r.Binary[pl]
			if b == nil || ri.GoVersion != "" {
				continue
			}
			ri.GoVersion = b.GoVersion
			ri.PluginSDK = b.Deps["github.com/hashicorp/terraform-plugin-sdk/v2"]
			ri.Framework = b.Deps["github.com/hashicorp/terraform-plugin-framework"]
			if prevDeps != nil {
				for d := range b.Deps {
					if _, ok := prevDeps[d]; !ok {
						ri.DepsAdded = append(ri.DepsAdded, strings.TrimPrefix(d, "github.com/"))
					}
				}
				for d := range prevDeps {
					if _, ok := b.Deps[d]; !ok {
						ri.DepsGone = append(ri.DepsGone, strings.TrimPrefix(d, "github.com/"))
					}
				}
				slices.Sort(ri.DepsAdded)
				slices.Sort(ri.DepsGone)
			}
			prevDeps = b.Deps
		}
		if r.Schema != nil {
			ri.Terraform = r.Schema.TerraformVersion
		}
		if m, ok := r.Stages[results.StageSchema]; ok {
			ri.Host = m.Host
			if m.CPU != "" {
				ri.Host += " (" + m.CPU + ")"
			}
		}
		if r.Build != nil {
			ri.Toolchain = r.Build.GoToolchain
		}
		for s, m := range r.Stages {
			if m.Error != "" {
				ri.Stages[s] = "failed: " + m.Error
			} else {
				ri.Stages[s] = "ok"
			}
		}
		d.Releases = append(d.Releases, ri)
	}

	for _, pl := range plats {
		d.Composition[pl] = map[string]Composition{
			compModules: composition(all, pl, func(b *results.BinaryResult) map[string]int64 {
				out := map[string]int64{}
				for k, v := range b.TextByModule {
					out[shortModule(k)] = v
				}
				return out
			}),
			"services": composition(all, pl, func(b *results.BinaryResult) map[string]int64 { return b.TextByService }),
			"sections": composition(all, pl, func(b *results.BinaryResult) map[string]int64 { return b.Sections }),
		}
	}

	return d
}

// dropBackports applies provider.DropBackports to stored results.
func dropBackports(all []*results.Result) []*results.Result {
	rels := make([]provider.Release, 0, len(all))
	for _, r := range all {
		rels = append(rels, provider.Release{Version: r.Version, Date: r.Date})
	}
	keep := map[string]bool{}
	for _, r := range provider.DropBackports(rels) {
		keep[r.Version] = true
	}
	out := make([]*results.Result, 0, len(all))
	for _, r := range all {
		if keep[r.Version] {
			out = append(out, r)
		}
	}
	return out
}

func writeCSV(path string, d data) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := csv.NewWriter(f)

	header := []string{"version", "date", "go_version"}
	for _, m := range d.Metrics {
		header = append(header, m.Key)
	}
	if err := w.Write(header); err != nil {
		return err
	}
	for i, r := range d.Releases {
		row := []string{fmtVersion(r.Version), r.Date, r.GoVersion}
		for _, m := range d.Metrics {
			if v := m.Values[i]; v != nil {
				row = append(row, strconv.FormatFloat(*v, 'f', -1, 64))
			} else {
				row = append(row, "")
			}
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("writing csv: %w", err)
	}
	return nil
}

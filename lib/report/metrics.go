// Package report flattens per-release results into chartable series and renders reports/<provider>/ as a
// self-contained html page plus data.json and data.csv.
package report

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/katbyte/tf-provider-profile/lib/results"
)

// Metric is one chartable series across releases.
type Metric struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Unit  string `json:"unit"` // bytes, ms, s, count, pct, ratio, days
	Group string `json:"group"`
	Desc  string `json:"desc,omitempty"`
	UpIs  string `json:"up_is"` // bad | good | neutral: how to colour a delta
	// Values holds one entry per release, nil where the stage has not run.
	Values []*float64 `json:"values"`
}

type Group struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

var groups = []Group{
	{"binary", "Binary"},
	{"composition", "Binary composition"},
	{"startup", "Startup"},
	{"schema", "Schema"},
	{"source", "Source"},
	{"resources", "Resources"},
	{"docs", "Documentation"},
	{"deps", "Dependencies"},
	{"churn", "Release churn"},
	{"build", "Build & lint"},
}

type def struct {
	key, label, unit, group, desc string
	get                           func(r *results.Result) (float64, bool)
}

func f(v int) (float64, bool)     { return float64(v), true }
func f64(v int64) (float64, bool) { return float64(v), true }

func binaryDefs(platform string) []def {
	p := platform
	b := func(r *results.Result) *results.BinaryResult {
		if r.Binary == nil {
			return nil
		}
		return r.Binary[p]
	}
	pre := "binary." + p + "."
	suf := " (" + p + ")"
	defs := make([]def, 0, 15)
	defs = append(defs, []def{
		{pre + "zip_bytes", "Compressed size" + suf, "bytes", "binary", "release zip as downloaded", func(r *results.Result) (float64, bool) {
			if x := b(r); x != nil {
				return f64(x.ZipBytes)
			}
			return 0, false
		}},
		{pre + "binary_bytes", "Uncompressed size" + suf, "bytes", "binary", "extracted provider executable", func(r *results.Result) (float64, bool) {
			if x := b(r); x != nil {
				return f64(x.BinaryBytes)
			}
			return 0, false
		}},
		{pre + "compression_ratio", "Compression ratio" + suf, "ratio", "binary", "uncompressed / compressed", func(r *results.Result) (float64, bool) {
			if x := b(r); x != nil && x.ZipBytes > 0 {
				return float64(x.BinaryBytes) / float64(x.ZipBytes), true
			}
			return 0, false
		}},
		{pre + "funcs", "Functions" + suf, "funcs", "composition", "functions in the pclntab", func(r *results.Result) (float64, bool) {
			if x := b(r); x != nil && x.Funcs > 0 {
				return f(x.Funcs)
			}
			return 0, false
		}},
		{pre + "packages", "Packages" + suf, "packages", "composition", "packages with at least one function", func(r *results.Result) (float64, bool) {
			if x := b(r); x != nil && x.Packages > 0 {
				return f(x.Packages)
			}
			return 0, false
		}},
		{pre + "dep_count", "Modules linked" + suf, "modules", "deps", "dependency modules recorded in the build info", func(r *results.Result) (float64, bool) {
			if x := b(r); x != nil {
				return f(x.DepCount)
			}
			return 0, false
		}},
	}...)
	// text attributed to the modules that matter for the sdk migration and the plugin stack
	for _, mod := range []struct{ path, label string }{
		{"github.com/Azure/azure-sdk-for-go", "Azure/azure-sdk-for-go (legacy)"},
		{"github.com/Azure/go-autorest/autorest", "Azure/go-autorest (legacy)"},
		{"github.com/jackofallops/kermit", "jackofallops/kermit (legacy)"},
		{"github.com/tombuildsstuff/kermit", "tombuildsstuff/kermit (legacy)"},
		{"github.com/hashicorp/go-azure-sdk/resource-manager", "go-azure-sdk/resource-manager"},
		{"github.com/hashicorp/go-azure-sdk/sdk", "go-azure-sdk/sdk"},
		{"github.com/hashicorp/go-azure-sdk/data-plane", "go-azure-sdk/data-plane"},
		{"github.com/hashicorp/go-azure-helpers", "go-azure-helpers"},
		{"github.com/hashicorp/terraform-plugin-sdk/v2", "terraform-plugin-sdk/v2"},
		{"github.com/hashicorp/terraform-plugin-framework", "terraform-plugin-framework"},
	} {
		defs = append(defs, def{pre + "module." + strings.TrimPrefix(mod.path, "github.com/"), "Text: " + mod.label + suf, "bytes", "composition", "function bytes attributed to this module via the pclntab", func(r *results.Result) (float64, bool) {
			if x := b(r); x != nil {
				if v, ok := x.TextByModule[mod.path]; ok {
					return f64(v)
				}
				if x.Funcs > 0 {
					return 0, true // module not linked in this release: a real zero
				}
			}
			return 0, false
		}})
	}
	defs = append(defs, def{pre + "module.provider", "Text: provider itself" + suf, "bytes", "composition", "function bytes attributed to the provider's own module", func(r *results.Result) (float64, bool) {
		if x := b(r); x != nil {
			if v, ok := x.TextByModule[x.ModulePath]; ok {
				return f64(v)
			}
		}
		return 0, false
	}}, def{pre + "module.std", "Text: Go standard library" + suf, "bytes", "composition", "", func(r *results.Result) (float64, bool) {
		if x := b(r); x != nil {
			if v, ok := x.TextByModule["std"]; ok {
				return f64(v)
			}
		}
		return 0, false
	}})
	for _, sec := range []struct{ name, label string }{
		{"text", "text (code)"},
		{"rodata", "rodata"},
		{"pclntab", "pclntab"},
		{"typelink", "typelink"},
		{"itablink", "itablink"},
		{"data", "data"},
		{"noptrdata", "noptrdata"},
		{"bss", "bss"},
		{"noptrbss", "noptrbss"},
	} {
		defs = append(defs, def{pre + "section." + sec.name, "Section " + sec.label + suf, "bytes", "composition", "", func(r *results.Result) (float64, bool) {
			if x := b(r); x != nil {
				if v, ok := x.Sections[sec.name]; ok {
					return f64(v)
				}
			}
			return 0, false
		}})
	}
	return defs
}

// tfSupports reports whether the terraform that produced a schema result can express a schema kind at all: older
// versions silently omit them, which must read as unknown rather than zero.
func tfSupports(x *results.SchemaResult, minMinor int) bool {
	if x == nil {
		return false
	}
	parts := strings.SplitN(x.TerraformVersion, ".", 3)
	if len(parts) < 2 {
		return true
	}
	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(parts[1])
	return major > 1 || (major == 1 && minor >= minMinor)
}

// sdkKindDefs breaks each kind of definition (resource, data source, list resource, action, ephemeral resource)
// down by the sdk family the defining file imports.
func sdkKindDefs() []def {
	kinds := []struct{ id, label string }{
		{"resource", "Resources"}, {"data_source", "Data sources"}, {"list", "List resources"}, {"action", "Actions"}, {"ephemeral", "Ephemeral resources"},
	}
	classes := []struct{ id, label, upIs string }{
		{"go_azure_sdk", "go-azure-sdk", "good"}, {"both", "both SDKs", "bad"}, {"kermit", "kermit", "bad"}, {"track1", "azure-sdk-for-go (track1)", "bad"}, {"none", "neither SDK", "neutral"},
	}
	defs := make([]def, 0, len(kinds)*len(classes)+8)
	// share of resource + data source files on each sdk family, of those importing any family
	total := func(x *results.SourceResult) int {
		n := 0
		for _, k := range []string{"resource", "data_source"} {
			for _, c := range []string{"go_azure_sdk", "both", "kermit", "track1"} {
				n += x.SDKByKind[k][c]
			}
		}
		return n
	}
	for _, c := range classes {
		defs = append(defs, def{"source.sdk_count." + c.id, "Resources on " + c.label, "files", "deps", "resource and data source files importing this sdk family", func(r *results.Result) (float64, bool) {
			x := r.Source
			if x == nil || x.SDKByKind == nil {
				return 0, false
			}
			return f(x.SDKByKind["resource"][c.id] + x.SDKByKind["data_source"][c.id])
		}})
	}
	for _, c := range classes[:4] {
		defs = append(defs, def{"source.sdk_pct." + c.id, "Resources on " + c.label + " share", "pct", "deps", "share of resource and data source files importing an sdk family", func(r *results.Result) (float64, bool) {
			x := r.Source
			if x == nil || x.SDKByKind == nil {
				return 0, false
			}
			t := total(x)
			if t == 0 {
				return 0, false
			}
			return 100 * float64(x.SDKByKind["resource"][c.id]+x.SDKByKind["data_source"][c.id]) / float64(t), true
		}})
	}
	for _, k := range kinds {
		for _, c := range classes {
			defs = append(defs, def{"source.sdk." + k.id + "." + c.id, k.label + " on " + c.label, "files", "deps", "files defining one, by the sdk family they import", func(r *results.Result) (float64, bool) {
				x := r.Source
				if x == nil || x.SDKByKind == nil {
					return 0, false
				}
				return f(x.SDKByKind[k.id][c.id])
			}})
		}
	}
	return defs
}

// schemaInt / startupF / sourceInt build the common "one field of a stage result" getters; the ok flag lets fields that
// were added later (zero in older result files) read as unknown rather than zero.
func schemaInt(get func(*results.SchemaResult) int, ok func(*results.SchemaResult) bool) func(*results.Result) (float64, bool) {
	return func(r *results.Result) (float64, bool) {
		if x := r.Schema; x != nil && (ok == nil || ok(x)) {
			return f(get(x))
		}
		return 0, false
	}
}

func sourceInt(get func(*results.SourceResult) int, ok func(*results.SourceResult) bool) func(*results.Result) (float64, bool) {
	return func(r *results.Result) (float64, bool) {
		if x := r.Source; x != nil && (ok == nil || ok(x)) {
			return f(get(x))
		}
		return 0, false
	}
}

func sourcePct(num, den func(*results.SourceResult) int) func(*results.Result) (float64, bool) {
	return func(r *results.Result) (float64, bool) {
		if x := r.Source; x != nil && den(x) > 0 {
			return 100 * float64(num(x)) / float64(den(x)), true
		}
		return 0, false
	}
}

// flagsKnown: the attribute flag counters exist once any of them is non-zero (required is never zero for a real schema)
func flagsKnown(x *results.SchemaResult) bool { return x.AttrsRequired > 0 || x.AttrsOptional > 0 }

// debtKnown: the debt counters were added together with the per-service ones; a tree with resources always has services
func debtKnown(x *results.SourceResult) bool { return x.ServicesWithResources > 0 }

func fixedDefs() []def {
	st := func(r *results.Result) *results.StartupResult { return r.Startup }
	sc := func(r *results.Result) *results.SchemaResult { return r.Schema }
	so := func(r *results.Result) *results.SourceResult { return r.Source }
	bu := func(r *results.Result) *results.BuildResult { return r.Build }
	li := func(r *results.Result) *results.LintResult { return r.Lint }
	te := func(r *results.Result) *results.TestResult { return r.Test }
	pc := func(r *results.Result) *results.PRCheckResult { return r.PRCheck }

	return []def{
		{"startup.handshake_med_ms", "Plugin handshake (median)", "ms", "startup", "time from exec to go-plugin handshake line, no terraform", func(r *results.Result) (float64, bool) {
			if x := st(r); x != nil {
				return x.HandshakeMedMs, true
			}
			return 0, false
		}},
		{"startup.handshake_min_ms", "Plugin handshake (min)", "ms", "startup", "", func(r *results.Result) (float64, bool) {
			if x := st(r); x != nil {
				return x.HandshakeMinMs, true
			}
			return 0, false
		}},
		{"startup.max_rss_bytes", "RSS at handshake", "bytes", "startup", "peak resident memory of the provider at handshake", func(r *results.Result) (float64, bool) {
			if x := st(r); x != nil && x.MaxRSSBytes > 0 {
				return f64(x.MaxRSSBytes)
			}
			return 0, false
		}},

		{"startup.init_ms", "Package init time", "ms", "startup", "sum of every package init before main (GODEBUG=inittrace=1), cheapest of 3 runs", func(r *results.Result) (float64, bool) {
			if x := st(r); x != nil && x.InitPackages > 0 {
				return x.InitMs, true
			}
			return 0, false
		}},
		{"startup.init_heap_bytes", "Package init heap", "bytes", "startup", "bytes allocated during package init", func(r *results.Result) (float64, bool) {
			if x := st(r); x != nil && x.InitPackages > 0 {
				return f64(x.InitHeapBytes)
			}
			return 0, false
		}},
		{"startup.init_allocs", "Package init allocations", "allocs", "startup", "allocations during package init", func(r *results.Result) (float64, bool) {
			if x := st(r); x != nil && x.InitPackages > 0 {
				return f64(x.InitAllocs)
			}
			return 0, false
		}},
		{"startup.init_packages", "Packages with init work", "packages", "startup", "packages the runtime reported an init for", func(r *results.Result) (float64, bool) {
			if x := st(r); x != nil && x.InitPackages > 0 {
				return f(x.InitPackages)
			}
			return 0, false
		}},

		{"schema.wall_med_ms", "Schema fetch wall time (median)", "ms", "schema", "terraform providers schema -json, end to end", func(r *results.Result) (float64, bool) {
			if x := sc(r); x != nil {
				return x.WallMedMs, true
			}
			return 0, false
		}},
		{"schema.wall_min_ms", "Schema fetch wall time (min)", "ms", "schema", "", func(r *results.Result) (float64, bool) {
			if x := sc(r); x != nil {
				return x.WallMinMs, true
			}
			return 0, false
		}},
		{"schema.provider_max_rss_bytes", "Provider RSS during schema fetch", "bytes", "schema", "peak resident memory of the provider process serving GetProviderSchema", func(r *results.Result) (float64, bool) {
			if x := sc(r); x != nil && x.ProviderMaxRSSBytes > 0 {
				return f64(x.ProviderMaxRSSBytes)
			}
			return 0, false
		}},
		{"schema.json_bytes", "Schema JSON size", "bytes", "schema", "", func(r *results.Result) (float64, bool) {
			if x := sc(r); x != nil {
				return f64(x.JSONBytes)
			}
			return 0, false
		}},
		{"schema.gzip_bytes", "Schema JSON size (gzipped)", "bytes", "schema", "", func(r *results.Result) (float64, bool) {
			if x := sc(r); x != nil {
				return f64(x.GzipBytes)
			}
			return 0, false
		}},
		{"schema.resources", "Resources (schema)", "resources", "resources", "", func(r *results.Result) (float64, bool) {
			if x := sc(r); x != nil {
				return f(x.Resources)
			}
			return 0, false
		}},
		{"schema.data_sources", "Data sources (schema)", "resources", "resources", "", func(r *results.Result) (float64, bool) {
			if x := sc(r); x != nil {
				return f(x.DataSources)
			}
			return 0, false
		}},
		{"schema.ephemeral_resources", "Ephemeral resources (schema)", "resources", "resources", "needs terraform >= 1.10 for the schema stage", func(r *results.Result) (float64, bool) {
			if x := sc(r); tfSupports(x, 10) {
				return f(x.EphemeralResources)
			}
			return 0, false
		}},
		{"schema.list_resources", "List resources (schema)", "resources", "resources", "needs terraform >= 1.14 for the schema stage", func(r *results.Result) (float64, bool) {
			if x := sc(r); tfSupports(x, 14) {
				return f(x.ListResources)
			}
			return 0, false
		}},
		{"schema.actions", "Actions (schema)", "resources", "resources", "needs terraform >= 1.14 for the schema stage", func(r *results.Result) (float64, bool) {
			if x := sc(r); tfSupports(x, 14) {
				return f(x.Actions)
			}
			return 0, false
		}},
		{"schema.functions", "Functions (schema)", "resources", "resources", "needs terraform >= 1.8 for the schema stage", func(r *results.Result) (float64, bool) {
			if x := sc(r); tfSupports(x, 8) {
				return f(x.Functions)
			}
			return 0, false
		}},
		{"schema.identity_resources", "Resources with identity (schema)", "resources", "resources", "resources exposing a resource identity schema; needs terraform >= 1.12", func(r *results.Result) (float64, bool) {
			if x := sc(r); tfSupports(x, 12) {
				return f(x.IdentityResources)
			}
			return 0, false
		}},
		{"schema.identity_coverage_pct", "Identity coverage", "pct", "resources", "resources with a resource identity schema / all resources", func(r *results.Result) (float64, bool) {
			if x := sc(r); tfSupports(x, 12) && x.Resources > 0 {
				return 100 * float64(x.IdentityResources) / float64(x.Resources), true
			}
			return 0, false
		}},
		{"schema.list_coverage_pct", "List coverage", "pct", "resources", "list resources / all resources", func(r *results.Result) (float64, bool) {
			if x := sc(r); tfSupports(x, 14) && x.Resources > 0 {
				return 100 * float64(x.ListResources) / float64(x.Resources), true
			}
			return 0, false
		}},
		{"schema.attributes", "Schema attributes", "attributes", "schema", "attributes across all resource and data source schemas, nested blocks included", func(r *results.Result) (float64, bool) {
			if x := sc(r); x != nil {
				return f(x.Attributes)
			}
			return 0, false
		}},
		{"schema.blocks", "Schema blocks", "attributes", "schema", "", func(r *results.Result) (float64, bool) {
			if x := sc(r); x != nil {
				return f(x.Blocks)
			}
			return 0, false
		}},
		{"schema.deprecated_attributes", "Deprecated attributes", "attributes", "schema", "", func(r *results.Result) (float64, bool) {
			if x := sc(r); x != nil {
				return f(x.DeprecatedAttrs)
			}
			return 0, false
		}},
		{"schema.max_depth", "Max block nesting", "depth", "schema", "", func(r *results.Result) (float64, bool) {
			if x := sc(r); x != nil {
				return f(x.MaxDepth)
			}
			return 0, false
		}},
		{"schema.attrs_required", "Required attributes", "attributes", "schema", "", schemaInt(func(x *results.SchemaResult) int { return x.AttrsRequired }, flagsKnown)},
		{"schema.attrs_optional", "Optional attributes", "attributes", "schema", "", schemaInt(func(x *results.SchemaResult) int { return x.AttrsOptional }, flagsKnown)},
		{"schema.attrs_computed", "Computed attributes", "attributes", "schema", "", schemaInt(func(x *results.SchemaResult) int { return x.AttrsComputed }, flagsKnown)},
		{"schema.attrs_sensitive", "Sensitive attributes", "attributes", "schema", "", schemaInt(func(x *results.SchemaResult) int { return x.AttrsSensitive }, flagsKnown)},
		{"schema.attrs_write_only", "Write-only attributes", "attributes", "schema", "needs terraform >= 1.11 for the schema stage", schemaInt(func(x *results.SchemaResult) int { return x.AttrsWriteOnly }, func(x *results.SchemaResult) bool { return flagsKnown(x) && tfSupports(x, 11) })},
		{"schema.attrs_described", "Described attributes", "attributes", "schema", "attributes carrying a description", schemaInt(func(x *results.SchemaResult) int { return x.AttrsDescribed }, flagsKnown)},
		{"schema.attrs_described_pct", "Description coverage", "pct", "schema", "described attributes / all attributes", func(r *results.Result) (float64, bool) {
			if x := sc(r); x != nil && flagsKnown(x) && x.Attributes > 0 {
				return 100 * float64(x.AttrsDescribed) / float64(x.Attributes), true
			}
			return 0, false
		}},
		{"schema.resources_with_migrations", "Resources with state migrations", "resources", "resources", "resources whose schema version is above 0", schemaInt(func(x *results.SchemaResult) int { return x.ResourcesWithMigrations }, flagsKnown)},
		{"schema.migration_pct", "State migration share", "pct", "resources", "resources with a schema version above 0 / all resources", func(r *results.Result) (float64, bool) {
			if x := sc(r); x != nil && flagsKnown(x) && x.Resources > 0 {
				return 100 * float64(x.ResourcesWithMigrations) / float64(x.Resources), true
			}
			return 0, false
		}},
		{"schema.resource_attrs_max", "Largest resource (attributes)", "attributes", "schema", "attributes of the biggest resource schema, nested included", schemaInt(func(x *results.SchemaResult) int { return x.ResourceAttrsMax }, flagsKnown)},
		{"schema.resource_attrs_median", "Median resource size (attributes)", "attributes", "schema", "", schemaInt(func(x *results.SchemaResult) int { return x.ResourceAttrsMedian }, flagsKnown)},
		{"schema.resource_attrs_mean", "Mean resource size (attributes)", "attributes", "schema", "resource attributes / resources", func(r *results.Result) (float64, bool) {
			if x := sc(r); x != nil && flagsKnown(x) && x.Resources > 0 {
				return float64(x.Attributes) / float64(x.Resources+x.DataSources), true
			}
			return 0, false
		}},

		{"source.go_lines", "Go lines", "lines", "source", "all .go files outside vendor", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.GoLines)
			}
			return 0, false
		}},
		{"source.go_code_lines", "Go code lines", "lines", "source", "non-blank, non-comment lines outside vendor", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.GoCodeLines)
			}
			return 0, false
		}},
		{"source.go_test_lines", "Go test lines", "lines", "source", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.GoTestLines)
			}
			return 0, false
		}},
		{"source.go_files", "Go files", "files", "source", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.GoFiles)
			}
			return 0, false
		}},
		{"source.go_test_files", "Go test files", "files", "source", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.GoTestFiles)
			}
			return 0, false
		}},
		{"source.vendor_go_lines", "Vendored Go lines", "lines", "deps", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.VendorGoLines)
			}
			return 0, false
		}},
		{"source.vendor_go_files", "Vendored Go files", "files", "deps", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.VendorGoFiles)
			}
			return 0, false
		}},
		{"source.total_files", "Files in tree", "files", "source", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.TotalFiles)
			}
			return 0, false
		}},
		{"source.tree_bytes", "Tree size", "bytes", "source", "working tree excluding .git", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f64(x.TreeBytes)
			}
			return 0, false
		}},
		{"source.services", "Service packages", "packages", "source", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.Services)
			}
			return 0, false
		}},
		{"source.test_funcs", "Test functions", "funcs", "source", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.TestFuncs)
			}
			return 0, false
		}},
		{"source.acc_test_funcs", "Acceptance test functions", "funcs", "source", "func TestAcc*", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.AccTestFuncs)
			}
			return 0, false
		}},
		{"source.resource_files_legacy_sdk", "Resource files on legacy SDK", "files", "deps", "resource/data source files importing azure-sdk-for-go or kermit only", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && x.ResourceFilesLegacySDK+x.ResourceFilesGoAzureSDK+x.ResourceFilesBothSDK > 0 {
				return f(x.ResourceFilesLegacySDK)
			}
			return 0, false
		}},
		{"source.resource_files_both_sdk", "Resource files on both SDKs", "files", "deps", "resource/data source files importing legacy and go-azure-sdk", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && x.ResourceFilesLegacySDK+x.ResourceFilesGoAzureSDK+x.ResourceFilesBothSDK > 0 {
				return f(x.ResourceFilesBothSDK)
			}
			return 0, false
		}},
		{"source.resource_files_go_azure_sdk", "Resource files on go-azure-sdk", "files", "deps", "resource/data source files importing go-azure-sdk only", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && x.ResourceFilesLegacySDK+x.ResourceFilesGoAzureSDK+x.ResourceFilesBothSDK > 0 {
				return f(x.ResourceFilesGoAzureSDK)
			}
			return 0, false
		}},
		{"source.resource_files_no_sdk", "Resource files on neither SDK", "files", "deps", "resource/data source files importing neither family", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && x.ResourceFilesLegacySDK+x.ResourceFilesGoAzureSDK+x.ResourceFilesBothSDK > 0 {
				return f(x.ResourceFilesNoSDK)
			}
			return 0, false
		}},
		{"source.identity_resource_files", "Resource files with identity", "files", "resources", "resource files declaring a resource identity in source", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && x.SDKByKind != nil {
				return f(x.IdentityResourceFiles)
			}
			return 0, false
		}},
		{"source.deprecated_resource_files", "Deprecated resources", "files", "resources", "resource and data source files carrying a resource-level deprecation", sourceInt(func(x *results.SourceResult) int { return x.DeprecatedResourceFiles }, debtKnown)},
		{"source.resource_files_without_tests", "Resources without a test file", "files", "resources", "resource and data source files with no sibling _test.go", sourceInt(func(x *results.SourceResult) int { return x.ResourceFilesWithoutTests }, debtKnown)},
		{"source.resource_files_total", "Resource files", "files", "resources", "files defining a resource or data source", sourceInt(func(x *results.SourceResult) int { return x.ResourceFilesTotal }, debtKnown)},
		{"source.nolint_directives", "nolint directives", "entries", "source", "//nolint comments in non-vendor go files", sourceInt(func(x *results.SourceResult) int { return x.NolintDirectives }, debtKnown)},
		{"source.todos", "TODO / FIXME mentions", "entries", "source", "in non-vendor go files", sourceInt(func(x *results.SourceResult) int { return x.Todos }, debtKnown)},
		{"source.services_with_resources", "Services with resources", "packages", "resources", "service packages holding at least one resource or data source file", sourceInt(func(x *results.SourceResult) int { return x.ServicesWithResources }, debtKnown)},
		{"source.services_fully_typed", "Fully typed services", "packages", "resources", "services with no untyped resource or data source left", sourceInt(func(x *results.SourceResult) int { return x.ServicesFullyTyped }, debtKnown)},
		{"source.services_fully_typed_pct", "Fully typed services share", "pct", "resources", "fully typed services / services with resources", sourcePct(func(x *results.SourceResult) int { return x.ServicesFullyTyped }, func(x *results.SourceResult) int { return x.ServicesWithResources })},
		{"source.services_fully_go_azure_sdk", "Fully go-azure-sdk services", "packages", "resources", "services with no resource file importing a legacy sdk", sourceInt(func(x *results.SourceResult) int { return x.ServicesFullyGoAzureSDK }, debtKnown)},
		{"source.services_fully_go_azure_sdk_pct", "Fully go-azure-sdk services share", "pct", "resources", "fully go-azure-sdk services / services with resources", sourcePct(func(x *results.SourceResult) int { return x.ServicesFullyGoAzureSDK }, func(x *results.SourceResult) int { return x.ServicesWithResources })},
		{"source.files_importing_legacy_sdk", "Files importing azure-sdk-for-go (legacy)", "files", "deps", "non-test service files", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && x.FilesImportingGoAzureSDK+x.FilesImportingLegacySDK > 0 {
				return f(x.FilesImportingLegacySDK)
			}
			return 0, false
		}},
		{"source.files_importing_kermit", "Files importing kermit (legacy)", "files", "deps", "non-test service files", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && x.FilesImportingGoAzureSDK+x.FilesImportingLegacySDK > 0 {
				return f(x.FilesImportingKermit)
			}
			return 0, false
		}},
		{"source.files_importing_autorest", "Files importing go-autorest (legacy)", "files", "deps", "non-test service files", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && x.FilesImportingGoAzureSDK+x.FilesImportingLegacySDK > 0 {
				return f(x.FilesImportingAutorest)
			}
			return 0, false
		}},
		{"source.files_importing_go_azure_sdk", "Files importing go-azure-sdk", "files", "deps", "non-test service files", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && x.FilesImportingGoAzureSDK+x.FilesImportingLegacySDK > 0 {
				return f(x.FilesImportingGoAzureSDK)
			}
			return 0, false
		}},

		{"source.typed_resources", "Typed resources", "resources", "resources", "files declaring sdk.Resource*", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.TypedResources)
			}
			return 0, false
		}},
		{"source.untyped_resources", "Untyped resources", "resources", "resources", "func resource*() *pluginsdk.Resource", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.UntypedResources)
			}
			return 0, false
		}},
		{"source.typed_resource_pct", "Typed resources share", "pct", "resources", "typed / (typed + untyped)", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && x.TypedResources+x.UntypedResources > 0 {
				return 100 * float64(x.TypedResources) / float64(x.TypedResources+x.UntypedResources), true
			}
			return 0, false
		}},
		{"source.typed_data_source_pct", "Typed data sources share", "pct", "resources", "typed / (typed + untyped) data sources", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && x.TypedDataSources+x.UntypedDataSources > 0 {
				return 100 * float64(x.TypedDataSources) / float64(x.TypedDataSources+x.UntypedDataSources), true
			}
			return 0, false
		}},
		{"source.typed_pct", "Typed share (resources + data sources)", "pct", "resources", "typed / all resources and data sources", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && x.TypedResources+x.UntypedResources+x.TypedDataSources+x.UntypedDataSources > 0 {
				return 100 * float64(x.TypedResources+x.TypedDataSources) / float64(x.TypedResources+x.UntypedResources+x.TypedDataSources+x.UntypedDataSources), true
			}
			return 0, false
		}},
		{"source.typed_data_sources", "Typed data sources", "resources", "resources", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.TypedDataSources)
			}
			return 0, false
		}},
		{"source.untyped_data_sources", "Untyped data sources", "resources", "resources", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.UntypedDataSources)
			}
			return 0, false
		}},
		{"source.docs_resources", "Resources", "resources", "docs", "website/docs/r", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.DocsResources)
			}
			return 0, false
		}},
		{"source.docs_data_sources", "Data sources", "resources", "docs", "website/docs/d", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.DocsDataSources)
			}
			return 0, false
		}},
		{"source.docs_ephemeral", "Ephemeral resources", "resources", "docs", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.DocsEphemeral)
			}
			return 0, false
		}},
		{"source.docs_list_resources", "List resources", "resources", "docs", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.DocsListResources)
			}
			return 0, false
		}},
		{"source.docs_actions", "Actions", "resources", "docs", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.DocsActions)
			}
			return 0, false
		}},
		{"source.docs_functions", "Functions", "resources", "docs", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.DocsFunctions)
			}
			return 0, false
		}},

		{"source.docs_guides", "Guides", "resources", "docs", "website/docs/guides", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.DocsGuides)
			}
			return 0, false
		}},
		{"source.direct_deps", "Direct dependencies", "modules", "deps", "go.mod require without // indirect", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.DirectDeps)
			}
			return 0, false
		}},
		{"source.indirect_deps", "Indirect dependencies", "modules", "deps", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.IndirectDeps)
			}
			return 0, false
		}},
		{"source.go_sum_lines", "go.sum lines", "lines", "deps", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.GoSumLines)
			}
			return 0, false
		}},

		{"source.commits", "Commits since previous release", "commits", "churn", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && x.PrevVersion != "" {
				return f(x.Commits)
			}
			return 0, false
		}},
		{"source.authors", "Authors since previous release", "authors", "churn", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && x.PrevVersion != "" {
				return f(x.Authors)
			}
			return 0, false
		}},
		{"source.files_changed", "Files changed since previous release", "files", "churn", "excluding vendor and CHANGELOG.md", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && x.PrevVersion != "" {
				return f(x.FilesChanged)
			}
			return 0, false
		}},
		{"source.lines_added", "Lines added since previous release", "lines", "churn", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && x.PrevVersion != "" {
				return f(x.LinesAdded)
			}
			return 0, false
		}},
		{"source.lines_removed", "Lines removed since previous release", "lines", "churn", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && x.PrevVersion != "" {
				return f(x.LinesRemoved)
			}
			return 0, false
		}},
		{"source.release_lag_hours", "Tag to release lag", "hours", "churn", "github publish time minus the tagged commit's time", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && !x.TagDate.IsZero() {
				return x.ReleaseLagHours, true
			}
			return 0, false
		}},
		{"source.days_since_prev", "Days since previous release", "days", "churn", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil && x.PrevVersion != "" {
				return x.DaysSincePrev, true
			}
			return 0, false
		}},
		{"source.changelog_total", "Changelog entries", "entries", "churn", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.ChangelogEntries["total"])
			}
			return 0, false
		}},
		{"source.changelog_features", "Changelog: features", "entries", "churn", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.ChangelogEntries["features"])
			}
			return 0, false
		}},
		{"source.changelog_enhancements", "Changelog: enhancements", "entries", "churn", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.ChangelogEntries["enhancements"])
			}
			return 0, false
		}},
		{"source.changelog_bug_fixes", "Changelog: bug fixes", "entries", "churn", "", func(r *results.Result) (float64, bool) {
			if x := so(r); x != nil {
				return f(x.ChangelogEntries["bug_fixes"])
			}
			return 0, false
		}},

		{"build.clean_build_s", "Clean build time", "s", "build", "go build with an empty GOCACHE (sampled)", func(r *results.Result) (float64, bool) {
			if x := bu(r); x != nil {
				return x.CleanBuildS, true
			}
			return 0, false
		}},
		{"build.warm_build_s", "Warm rebuild time", "s", "build", "go build again with everything cached: link cost (sampled)", func(r *results.Result) (float64, bool) {
			if x := bu(r); x != nil {
				return x.WarmBuildS, true
			}
			return 0, false
		}},
		{"build.local_binary_bytes", "Local build size", "bytes", "build", "unstripped dev build (sampled)", func(r *results.Result) (float64, bool) {
			if x := bu(r); x != nil {
				return f64(x.LocalBinaryBytes)
			}
			return 0, false
		}},
		{"build.stripped_binary_bytes", "Local build size (stripped)", "bytes", "build", "-ldflags=-s -w (sampled)", func(r *results.Result) (float64, bool) {
			if x := bu(r); x != nil {
				return f64(x.StrippedBinaryBytes)
			}
			return 0, false
		}},
		{"lint.duration_s", "Lint time", "s", "build", "cold golangci-lint run (sampled)", func(r *results.Result) (float64, bool) {
			if x := li(r); x != nil {
				return x.DurationS, true
			}
			return 0, false
		}},
		{"lint.issues", "Lint issues", "count", "build", "", func(r *results.Result) (float64, bool) {
			if x := li(r); x != nil {
				return f(x.Issues)
			}
			return 0, false
		}},
		{"test.duration_s", "Unit test time", "s", "build", "the provider's own unit test run (sampled)", func(r *results.Result) (float64, bool) {
			if x := te(r); x != nil {
				return x.DurationS, true
			}
			return 0, false
		}},
		{"test.failures", "Unit test failures", "count", "build", "packages reported FAIL (sampled)", func(r *results.Result) (float64, bool) {
			if x := te(r); x != nil {
				return f(x.Failures)
			}
			return 0, false
		}},
		{"prcheck.duration_s", "PR checks time", "s", "build", "make pr-check where the release has it: the full PR gate, every lint flavour included; lint time covers the releases without it (sampled)", func(r *results.Result) (float64, bool) {
			if x := pc(r); x != nil {
				return x.DurationS, true
			}
			return 0, false
		}},

		// a release that broke mid-stage leaves a hole in the timing series that looks identical to one never
		// profiled, so the breakage gets its own series; the reason is in stages.<name>.error of each result
		{"build.stage_failed", "Build stage broke", "count", "build", "1 where the build stage errored out instead of producing a timing (reason in stages.build.error)", stageBroke(results.StageBuild)},
		{"test.stage_failed", "Test stage broke", "count", "build", "1 where the test run never produced package results, as opposed to tests reporting failures (reason in stages.test.error)", stageBroke(results.StageTest)},
		{"lint.stage_failed", "Lint stage broke", "count", "build", "1 where the lint run itself broke rather than reporting issues (reason in stages.lint.error)", stageBroke(results.StageLint)},
		{"prcheck.gate_failed", "PR checks failed", "count", "build", "1 where make pr-check exited non-zero: the gate does not pass at that tag (output tail in prcheck.fail_tail)", func(r *results.Result) (float64, bool) {
			if x := pc(r); x != nil {
				return b2f(x.ExitCode != 0)
			}
			return 0, false
		}},
	}
}

// stageBroke reports 1 for releases where the stage ran and errored and 0 where it ran cleanly, and nothing at all
// where it never ran, so a broken stage plots as a point rather than vanishing into the gaps.
func stageBroke(stage string) func(r *results.Result) (float64, bool) {
	return func(r *results.Result) (float64, bool) {
		m, ok := r.Stages[stage]
		if !ok {
			return 0, false
		}
		return b2f(m.Error != "")
	}
}

func b2f(b bool) (float64, bool) {
	if b {
		return 1, true
	}
	return 0, true
}

// upIs says whether growth in a metric is bad (size, time, memory, issues), good, or just a fact.
func upIs(d def) string {
	switch d.unit {
	case "bytes", "ms", "s":
		return "bad"
	}
	if strings.HasPrefix(d.key, "source.sdk.") || strings.HasPrefix(d.key, "source.sdk_pct.") || strings.HasPrefix(d.key, "source.sdk_count.") {
		switch {
		case strings.HasSuffix(d.key, ".kermit"), strings.HasSuffix(d.key, ".track1"), strings.HasSuffix(d.key, ".both"):
			return "bad"
		case strings.HasSuffix(d.key, ".go_azure_sdk"):
			return "good"
		}
		return "neutral"
	}
	switch d.key {
	case "schema.deprecated_attributes", "lint.issues", "test.failures", "source.untyped_resources", "source.untyped_data_sources",
		"build.stage_failed", "test.stage_failed", "lint.stage_failed", "prcheck.gate_failed",
		"source.deprecated_resource_files", "source.resource_files_without_tests", "source.nolint_directives", "source.todos",
		"startup.init_ms", "startup.init_heap_bytes", "startup.init_allocs",
		"source.files_importing_legacy_sdk", "source.files_importing_kermit", "source.files_importing_autorest",
		"source.resource_files_legacy_sdk", "source.resource_files_both_sdk":
		return "bad"
	case "source.typed_resource_pct", "source.typed_data_source_pct", "source.typed_pct", "source.typed_resources", "source.typed_data_sources",
		"schema.identity_coverage_pct", "schema.list_coverage_pct", "schema.identity_resources", "schema.list_resources",
		"schema.attrs_described", "schema.attrs_described_pct", "source.services_fully_typed", "source.services_fully_typed_pct",
		"source.services_fully_go_azure_sdk", "source.services_fully_go_azure_sdk_pct":
		return "good"
	}
	return "neutral"
}

// platforms returns every platform seen in the binary results, native-first then sorted.
func platforms(all []*results.Result, native string) []string {
	seen := map[string]bool{}
	for _, r := range all {
		for p := range r.Binary {
			seen[p] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b string) int {
		if (a == native) != (b == native) {
			if a == native {
				return -1
			}
			return 1
		}
		return cmp.Compare(a, b)
	})
	return out
}

func buildMetrics(all []*results.Result, plats []string) []Metric {
	defs := make([]def, 0, 15*len(plats)+80)
	for _, p := range plats {
		defs = append(defs, binaryDefs(p)...)
	}
	defs = append(defs, fixedDefs()...)
	defs = append(defs, sdkKindDefs()...)

	var ms []Metric
	for _, d := range defs {
		m := Metric{Key: d.key, Label: d.label, Unit: d.unit, Group: d.group, Desc: d.desc, UpIs: upIs(d), Values: make([]*float64, len(all))}
		found := false
		for i, r := range all {
			if v, ok := d.get(r); ok {
				m.Values[i] = new(v)
				found = true
			}
		}
		if found {
			ms = append(ms, m)
		}
	}
	return ms
}

// Composition is a set of named series (modules, services, sections) for stacked charts, per platform.
type Composition struct {
	Names []string     `json:"names"`
	Rows  [][]*float64 `json:"rows"` // one row per release, one entry per name
}

func composition(all []*results.Result, platform string, pick func(*results.BinaryResult) map[string]int64) Composition {
	names := map[string]int64{}
	for _, r := range all {
		if b := r.Binary[platform]; b != nil {
			for k, v := range pick(b) {
				names[k] = max(names[k], v)
			}
		}
	}
	ordered := make([]string, 0, len(names))
	for n := range names {
		ordered = append(ordered, n)
	}
	slices.SortFunc(ordered, func(a, b string) int {
		if names[a] != names[b] {
			return cmp.Compare(names[b], names[a])
		}
		return cmp.Compare(a, b)
	})

	c := Composition{Names: ordered, Rows: make([][]*float64, len(all))}
	for i, r := range all {
		row := make([]*float64, len(ordered))
		if b := r.Binary[platform]; b != nil {
			m := pick(b)
			for j, n := range ordered {
				if v, ok := m[n]; ok {
					row[j] = new(float64(v))
				}
			}
		}
		c.Rows[i] = row
	}
	return c
}

func shortModule(m string) string {
	m = strings.TrimPrefix(m, "github.com/")
	return m
}

func fmtVersion(v string) string { return strings.TrimPrefix(v, "v") }

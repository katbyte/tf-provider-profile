// Package results defines what tfpp records per release and the on-disk store (one json file per release under
// .cache/<provider>/results/).
package results

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/katbyte/tf-provider-profile/lib/provider"
)

// Stage names, in the order they run.
const (
	StageDownload = "download"
	StageBinary   = "binary"
	StageStartup  = "startup"
	StageSchema   = "schema"
	StageSource   = "source"
	StageBuild    = "build"
	StageTest     = "test"
	StageLint     = "lint"
	StagePRCheck  = "prcheck"
)

// AllStages lists every stage in run order.
var AllStages = []string{StageDownload, StageBinary, StageStartup, StageSchema, StageSource, StageBuild, StageTest, StageLint, StagePRCheck}

// ExpensiveStages are the stages sampled by --sample.
var ExpensiveStages = []string{StageBuild, StageTest, StageLint, StagePRCheck}

// StageMeta records when a stage ran and whether it failed.
type StageMeta struct {
	RanAt     time.Time `json:"ran_at"`
	DurationS float64   `json:"duration_s"`
	Error     string    `json:"error,omitempty"`
	Host      string    `json:"host,omitempty"`
	CPU       string    `json:"cpu,omitempty"` // cpu model, so timings from different machines are not compared blindly
	OS        string    `json:"os,omitempty"`  // GOOS/GOARCH
}

// BinaryResult is what the binary stage extracts from a release zip + binary for one platform.
type BinaryResult struct {
	Platform       string            `json:"platform"`
	ZipBytes       int64             `json:"zip_bytes"`
	BinaryBytes    int64             `json:"binary_bytes"`
	GoVersion      string            `json:"go_version"`
	ModulePath     string            `json:"module_path"`
	DepCount       int               `json:"dep_count"`
	Deps           map[string]string `json:"deps"`     // module path -> version (replacement applied)
	Settings       map[string]string `json:"settings"` // go build settings: -ldflags, CGO_ENABLED, vcs.*...
	Sections       map[string]int64  `json:"sections"` // normalised section name -> bytes
	Funcs          int               `json:"funcs"`
	Packages       int               `json:"packages"`
	TextAttributed int64             `json:"text_attributed"` // bytes of text attributed to functions via pclntab
	TextByModule   map[string]int64  `json:"text_by_module"`  // module (or std/main/unknown) -> function bytes
	TextByService  map[string]int64  `json:"text_by_service"` // provider internal/services/<svc> -> function bytes
	TextByPackage  map[string]int64  `json:"text_by_package"` // package -> function bytes (largest only)
}

// StartupResult measures the bare plugin handshake with no terraform involved.
type StartupResult struct {
	Platform        string  `json:"platform"`
	Runs            int     `json:"runs"`
	HandshakeMinMs  float64 `json:"handshake_min_ms"`
	HandshakeMedMs  float64 `json:"handshake_med_ms"`
	MaxRSSBytes     int64   `json:"max_rss_bytes"` // median across runs
	ProtocolVersion string  `json:"protocol_version"`

	// package init cost from GODEBUG=inittrace=1: everything that runs before main, the fixed part of every launch
	InitPackages  int         `json:"init_packages,omitempty"`   // packages with an init that took measurable time
	InitMs        float64     `json:"init_ms,omitempty"`         // sum of per-package init clock time, min across runs
	InitHeapBytes int64       `json:"init_heap_bytes,omitempty"` // bytes allocated during package init
	InitAllocs    int64       `json:"init_allocs,omitempty"`     // allocations during package init
	InitTop       []InitEntry `json:"init_top,omitempty"`        // the most expensive package inits
}

// InitEntry is one package's init cost.
type InitEntry struct {
	Package string  `json:"package"`
	Ms      float64 `json:"ms"`
	Bytes   int64   `json:"bytes"`
}

// NamedCount pairs a schema entity with a count, for top-n lists.
type NamedCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// SchemaResult measures `terraform providers schema -json` against the release.
type SchemaResult struct {
	Platform            string  `json:"platform"`
	TerraformVersion    string  `json:"terraform_version"`
	Runs                int     `json:"runs"`
	WallMinMs           float64 `json:"wall_min_ms"`
	WallMedMs           float64 `json:"wall_med_ms"`
	ProviderMaxRSSBytes int64   `json:"provider_max_rss_bytes"` // max across runs
	JSONBytes           int64   `json:"json_bytes"`
	GzipBytes           int64   `json:"gzip_bytes"`
	Resources           int     `json:"resources"`
	DataSources         int     `json:"data_sources"`
	EphemeralResources  int     `json:"ephemeral_resources"`
	ListResources       int     `json:"list_resources"`
	Actions             int     `json:"actions"`
	Functions           int     `json:"functions"`
	IdentityResources   int     `json:"identity_resources"` // resources with a resource identity schema
	Attributes          int     `json:"attributes"`         // across resources + data sources, nested included
	Blocks              int     `json:"blocks"`
	DeprecatedAttrs     int     `json:"deprecated_attributes"`
	MaxDepth            int     `json:"max_depth"`
	ProviderAttributes  int     `json:"provider_attributes"`

	// attribute flags across resource and data source schemas, nested included
	AttrsRequired  int `json:"attrs_required,omitempty"`
	AttrsOptional  int `json:"attrs_optional,omitempty"`
	AttrsComputed  int `json:"attrs_computed,omitempty"`
	AttrsSensitive int `json:"attrs_sensitive,omitempty"`
	AttrsWriteOnly int `json:"attrs_write_only,omitempty"`
	AttrsDescribed int `json:"attrs_described,omitempty"` // attributes carrying a description
	// resources whose schema version is above zero, i.e. shipping state upgraders
	ResourcesWithMigrations int `json:"resources_with_migrations,omitempty"`
	// attributes per resource (nested included): distribution and the largest resources
	ResourceAttrsMax    int          `json:"resource_attrs_max,omitempty"`
	ResourceAttrsMedian int          `json:"resource_attrs_median,omitempty"`
	LargestResources    []NamedCount `json:"largest_resources,omitempty"`
}

// SourceResult is what the source stage counts at the release tag.
type SourceResult struct {
	Commit string `json:"commit"`

	GoFiles       int   `json:"go_files"`
	GoLines       int   `json:"go_lines"`
	GoCodeLines   int   `json:"go_code_lines"` // non-blank, non-comment-only lines
	GoTestFiles   int   `json:"go_test_files"`
	GoTestLines   int   `json:"go_test_lines"`
	VendorGoFiles int   `json:"vendor_go_files"`
	VendorGoLines int   `json:"vendor_go_lines"`
	TotalFiles    int   `json:"total_files"`
	TreeBytes     int64 `json:"tree_bytes"` // working tree size excluding .git

	Services           int `json:"services"`
	TypedResources     int `json:"typed_resources"`
	TypedDataSources   int `json:"typed_data_sources"`
	UntypedResources   int `json:"untyped_resources"`
	UntypedDataSources int `json:"untyped_data_sources"`
	TestFuncs          int `json:"test_funcs"`
	AccTestFuncs       int `json:"acc_test_funcs"`

	// sdk migration: non-test files under the service packages importing each sdk family
	FilesImportingLegacySDK  int `json:"files_importing_legacy_sdk"`   // github.com/Azure/azure-sdk-for-go
	FilesImportingKermit     int `json:"files_importing_kermit"`       // */kermit (hand-maintained legacy clients)
	FilesImportingAutorest   int `json:"files_importing_autorest"`     // github.com/Azure/go-autorest
	FilesImportingGoAzureSDK int `json:"files_importing_go_azure_sdk"` // github.com/hashicorp/go-azure-sdk

	// resource/data source files (those defining one) classified by the sdk family they import
	ResourceFilesLegacySDK  int `json:"resource_files_legacy_sdk"`   // azure-sdk-for-go and/or kermit only
	ResourceFilesGoAzureSDK int `json:"resource_files_go_azure_sdk"` // go-azure-sdk only
	ResourceFilesBothSDK    int `json:"resource_files_both_sdk"`     // both families
	ResourceFilesNoSDK      int `json:"resource_files_no_sdk"`       // neither (other clients, helpers)

	// per kind (resource, data_source, list, action, ephemeral): files defining one, by sdk family
	// (go_azure_sdk, both, kermit, track1 = azure-sdk-for-go, none)
	SDKByKind map[string]map[string]int `json:"sdk_by_kind,omitempty"`
	// resource files declaring a resource identity (schema.ResourceIdentity or sdk.ResourceWithIdentity)
	IdentityResourceFiles int `json:"identity_resource_files"`
	// resource/data source files carrying a resource-level deprecation (DeprecationMessage: or sdk.ResourceWithDeprecation*)
	DeprecatedResourceFiles int `json:"deprecated_resource_files,omitempty"`
	// resource/data source files with no sibling _test.go
	ResourceFilesWithoutTests int `json:"resource_files_without_tests,omitempty"`
	ResourceFilesTotal        int `json:"resource_files_total,omitempty"` // every file defining a resource or data source
	// lint and tech-debt markers in non-vendor go files
	NolintDirectives int `json:"nolint_directives,omitempty"`
	Todos            int `json:"todos,omitempty"` // TODO / FIXME mentions
	// per-service migration progress: services with at least one resource file, and how many of them are done
	ServicesWithResources   int `json:"services_with_resources,omitempty"`
	ServicesFullyTyped      int `json:"services_fully_typed,omitempty"`        // no untyped resource or data source left
	ServicesFullyGoAzureSDK int `json:"services_fully_go_azure_sdk,omitempty"` // every sdk-importing resource file is go-azure-sdk only

	DocsResources     int `json:"docs_resources"`
	DocsDataSources   int `json:"docs_data_sources"`
	DocsEphemeral     int `json:"docs_ephemeral"`
	DocsListResources int `json:"docs_list_resources"`
	DocsActions       int `json:"docs_actions"`
	DocsFunctions     int `json:"docs_functions"`
	DocsGuides        int `json:"docs_guides"`

	GoDirective   string `json:"go_directive"`
	GoVersionFile string `json:"go_version_file"`
	DirectDeps    int    `json:"direct_deps"`
	IndirectDeps  int    `json:"indirect_deps"`
	GoSumLines    int    `json:"go_sum_lines"`

	PrevVersion      string         `json:"prev_version"`
	Commits          int            `json:"commits"` // since previous release
	Authors          int            `json:"authors"`
	FilesChanged     int            `json:"files_changed"`
	LinesAdded       int            `json:"lines_added"`
	LinesRemoved     int            `json:"lines_removed"`
	DaysSincePrev    float64        `json:"days_since_prev"`
	TagDate          time.Time      `json:"tag_date,omitzero"`           // commit date of the tagged commit
	ReleaseLagHours  float64        `json:"release_lag_hours,omitempty"` // github publish time minus tag commit time
	ChangelogEntries map[string]int `json:"changelog_entries"`           // section -> bullet count
}

// BuildResult times a local build at the release tag.
type BuildResult struct {
	GoToolchain         string  `json:"go_toolchain"`
	CleanBuildS         float64 `json:"clean_build_s"`
	WarmBuildS          float64 `json:"warm_build_s"` // rebuild with warm cache: link cost
	StrippedBuildS      float64 `json:"stripped_build_s"`
	LocalBinaryBytes    int64   `json:"local_binary_bytes"`
	StrippedBinaryBytes int64   `json:"stripped_binary_bytes"`
	VetS                float64 `json:"vet_s,omitempty"`
}

// LintResult times golangci-lint at the release tag.
type LintResult struct {
	Tool        string  `json:"tool"`
	ToolVersion string  `json:"tool_version"`
	DurationS   float64 `json:"duration_s"`
	ExitCode    int     `json:"exit_code"`
	Issues      int     `json:"issues"`
}

// TestResult times the provider's unit tests at the release tag.
type TestResult struct {
	Tool      string  `json:"tool"` // "make test" or "go test"
	DurationS float64 `json:"duration_s"`
	ExitCode  int     `json:"exit_code"`
	Packages  int     `json:"packages,omitempty"` // packages reported ok or FAIL
	Failures  int     `json:"failures,omitempty"` // packages reported FAIL
}

// PRCheckResult times the provider's aggregate pr-check make target, where one exists.
type PRCheckResult struct {
	Target    string  `json:"target"`
	DurationS float64 `json:"duration_s"`
	ExitCode  int     `json:"exit_code"`
}

// Result is everything recorded for one release.
type Result struct {
	Version string    `json:"version"`
	Date    time.Time `json:"date"`

	Stages  map[string]StageMeta     `json:"stages"`
	Binary  map[string]*BinaryResult `json:"binary,omitempty"` // by platform
	Startup *StartupResult           `json:"startup,omitempty"`
	Schema  *SchemaResult            `json:"schema,omitempty"`
	Source  *SourceResult            `json:"source,omitempty"`
	Build   *BuildResult             `json:"build,omitempty"`
	Test    *TestResult              `json:"test,omitempty"`
	Lint    *LintResult              `json:"lint,omitempty"`
	PRCheck *PRCheckResult           `json:"prcheck,omitempty"`
}

// Store reads and writes per-release result files.
type Store struct {
	Dir string
}

func NewStore(p *provider.Provider) *Store { return &Store{Dir: p.ResultsDir()} }

func (s *Store) path(version string) string { return filepath.Join(s.Dir, version+".json") }

// Load returns the stored result for a release, or a fresh one if none exists yet.
func (s *Store) Load(r provider.Release) (*Result, error) {
	res := &Result{Version: r.Version, Date: r.Date, Stages: map[string]StageMeta{}}
	b, err := os.ReadFile(s.path(r.Version))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return res, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, res); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", s.path(r.Version), err)
	}
	if res.Stages == nil {
		res.Stages = map[string]StageMeta{}
	}
	res.Date = r.Date
	return res, nil
}

// Save writes the result file atomically.
func (s *Store) Save(res *Result) error {
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(res.Version) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(res.Version))
}

// LoadAll returns every stored result, oldest release first.
func (s *Store) LoadAll() ([]*Result, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Result
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var r Result
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", e.Name(), err)
		}
		if r.Stages == nil {
			r.Stages = map[string]StageMeta{}
		}
		out = append(out, &r)
	}
	slices.SortFunc(out, func(a, b *Result) int { return provider.CompareVersion(a.Version, b.Version) })
	return out, nil
}

// Done reports whether a stage completed successfully for this release.
func (r *Result) Done(stage string) bool {
	m, ok := r.Stages[stage]
	return ok && m.Error == ""
}

// Failed reports whether a stage ran and recorded an error.
func (r *Result) Failed(stage string) bool {
	m, ok := r.Stages[stage]
	return ok && m.Error != ""
}

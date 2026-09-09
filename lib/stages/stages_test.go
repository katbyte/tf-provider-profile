package stages

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/katbyte/tf-provider-profile/lib/results"
)

func TestNormaliseSection(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		".text": "text", "__text": "text", ".gopclntab": "pclntab", "__gopclntab": "pclntab",
		".rdata": "rodata", "__rodata": "rodata", ".go.buildinfo": "buildinfo", "__go_buildinfo": "buildinfo",
		".debug_info": "debug", ".zdebug_line": "debug", "__got": "other", ".symtab": "symtab",
	}
	for in, want := range cases {
		if got := normaliseSection(in); got != want {
			t.Errorf("normaliseSection(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseChangelog(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	changelog := `## 5.4.0 (September 03, 2026)

FEATURES:

* **New Resource**: ` + "`azurerm_a`" + ` ([#1](x))
* **New Data Source**: ` + "`azurerm_b`" + ` ([#2](x))

BUG FIXES:

* ` + "`azurerm_c`" + ` - fix thing ([#3](x))

## 5.3.0 (August 27, 2026)

FEATURES:

* older entry
`
	if err := os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte(changelog), 0o600); err != nil {
		t.Fatal(err)
	}
	sr := &results.SourceResult{ChangelogEntries: map[string]int{}}
	parseChangelog(dir, "v5.4.0", sr)
	if sr.ChangelogEntries["features"] != 2 || sr.ChangelogEntries["bug_fixes"] != 1 || sr.ChangelogEntries["total"] != 3 {
		t.Errorf("unexpected changelog counts: %v", sr.ChangelogEntries)
	}
}

func TestParseSchema(t *testing.T) {
	t.Parallel()
	js := `{"provider_schemas":{"registry.terraform.io/hashicorp/azurerm":{
		"provider":{"block":{"attributes":{"a":{},"b":{}}}},
		"resource_schemas":{"azurerm_x":{"block":{"attributes":{"id":{},"old":{"deprecated":true}},"block_types":{"inner":{"block":{"attributes":{"c":{}},"block_types":{"deeper":{"block":{"attributes":{"d":{}}}}}}}}}}},
		"data_source_schemas":{"azurerm_y":{"block":{"attributes":{"id":{}}}}},
		"functions":{"f":{}}
	}}}`
	sr, err := parseSchema([]byte(js), "hashicorp/azurerm")
	if err != nil {
		t.Fatal(err)
	}
	if sr.Resources != 1 || sr.DataSources != 1 || sr.Functions != 1 || sr.ProviderAttributes != 2 {
		t.Errorf("counts: %+v", sr)
	}
	if sr.Attributes != 5 || sr.Blocks != 2 || sr.DeprecatedAttrs != 1 || sr.MaxDepth != 3 {
		t.Errorf("walk: attrs=%d blocks=%d deprecated=%d depth=%d", sr.Attributes, sr.Blocks, sr.DeprecatedAttrs, sr.MaxDepth)
	}
	if _, err := parseSchema([]byte(js), "hashicorp/aws"); err == nil {
		t.Error("expected error for missing provider")
	}
}

func TestCountLines(t *testing.T) {
	t.Parallel()
	total, code := countLines([]byte("package x\n\n// comment\nfunc f() {}\n"))
	if total != 4 || code != 2 {
		t.Errorf("countLines = %d, %d; want 4, 2", total, code)
	}
}

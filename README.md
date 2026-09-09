# tfpp - terraform provider profiler

`tfpp` downloads every release of a terraform provider and records how it has changed over time, then renders an
interactive report. It was written to answer "how has the azurerm provider's binary size changed" and grew into
everything that can be measured for free from a release zip, the provider's git tag, and a short terraform run.

## What it measures

| stage      | source            | cost        | records |
|------------|-------------------|-------------|---------|
| `download` | releases.hashicorp.com | network | release zips for each `--platforms` entry, extracted into `.cache/<provider>/binaries/<version>/<platform>/` |
| `binary`   | the zip + binary  | <1s         | compressed and uncompressed size, go version, linked modules and their versions, build settings, object-file sections (text, rodata, pclntab, ...), function and package counts, and text bytes attributed to every module, provider service package and large package (via the pclntab, which survives stripping) |
| `startup`  | the native binary | ~1s         | time from exec to the go-plugin handshake line and peak RSS at that point, no terraform involved; package init time, heap and allocations from `GODEBUG=inittrace=1` with the most expensive packages |
| `schema`   | terraform         | ~2s         | wall time of `terraform providers schema -json`, the provider's own peak RSS while serving it (terraform launches the provider through a `tfpp _wrap` shim), schema json size, and counts of resources, data sources, ephemeral/list resources, actions, functions, attributes, blocks, deprecated attributes and nesting depth; required/optional/computed/sensitive/write-only/described attribute counts, resources with state migrations (schema version > 0), and the largest resources by attribute count |
| `source`   | git checkout of the tag | ~10s  | go lines/files (tests, vendor and code-only split), tree size, service packages, typed vs untyped resources and data sources, test functions, deprecated resources, resource files without a test file, nolint and TODO counts, per-service typed and go-azure-sdk migration completion, tag-to-release lag, documented resources/data sources/ephemeral/list/actions/functions, go.mod direct/indirect deps, go.sum lines, go version, and since the previous release: commits, authors, files changed, lines added/removed, days elapsed, changelog entries by section |
| `build`    | git checkout      | minutes     | **sampled** - clean build time with an isolated, emptied `GOCACHE`, warm rebuild time (link cost), stripped build time, and the local unstripped/stripped binary sizes, using the go toolchain pinned by the release's `.go-version` |
| `lint`     | git checkout      | many minutes | **sampled** - cold `golangci-lint run ./...` time and issue count, using the provider's own custom golangci binary when it has one (azurerm's `make golangci-with-modules`), otherwise the one on PATH |

Timing stages run against the native platform's binary. Everything is stored as one json per release under
`.cache/<provider>/results/` and stages that already completed are skipped on rerun, so `tfpp run` can be run
whenever a new release appears.

## Usage

```sh
make build

# everything since 2024-07-01 (the default), build+lint for every 4th release
./tfpp run --sample 4

# just the cheap stages, all releases
./tfpp run --stages download,binary,startup,schema,source

# one release, redo it
./tfpp run --versions v5.4.0 --force

# what has and hasn't run
./tfpp releases

# regenerate the report from cached results
./tfpp report
open reports/azurerm/index.html
```

The providers to profile are listed in `.tfpp.yml` (name and github repo); `tfpp run` without `--provider` loops
over all of them, and so does the weekly pages workflow. The same file holds shared flag defaults keyed by flag name.
Machine specific settings go in the gitignored `.tfpp.local.yml`, which is merged over it, e.g.
`terraform: .cache/tools/terraform/terraform`; `TFPP_*` environment variables and flags override both.

The schema stage uses whatever `terraform` is on PATH unless `--terraform` points elsewhere. Older terraform
versions do not include ephemeral/list resources, actions or functions in `providers schema -json`, so for those
counts use a current terraform; the version used is recorded with each result.

Other providers work too: `tfpp -p aws run`, or add them to `.tfpp.yml`. The source stage's typed/untyped resource patterns are azurerm's, and
report as zero elsewhere.

## Report

`reports/index.html` is the combined page for every provider under `data/`; `reports/<provider>/` holds the same
page for one provider plus `data.json` and `data.csv`. All open from disk.

The sidebar is a tree: **enabled** lists what is on the page, **presets** add bundles (size, memory, speed, binary
make-up, schema, source, typed migration, churn, build & lint) as combined charts, and the catalogue below is
collapsed by area and platform. Every metric has a unit (bytes, ms, s, count, pct, ...) which is the "value type"
used to decide what can share an axis.

Views:

- **panels** - one chart per group. Drag a panel onto another to combine them into one chart (same unit shares the
  axis, a second unit goes on a right axis), **stack** piles same-unit metrics on top of each other, **split**
  separates them again, and the x in a legend entry removes a metric.
- **one chart per unit** - everything selected, grouped by unit on real axes.
- **two axes** - everything on one chart with L/R toggles per metric under "enabled".
- **% change** - every metric relative to its first release, for comparing shapes.
- **per-release change** - bars of what each release changed, with the largest jump called out.

Right-click any point to **ask an AI** about it: the menu builds a prompt with the hovered chart's numbers for that
release and the previous one, the markers that fired (toolchain, dependency, sdk changes), and the diff, changelog
and release links, then hands it to Claude, ChatGPT or Perplexity in a new tab, or copies it for anything else.

The provider dropdown top-left switches between providers, or **all**, which puts each provider at the top of the
tree and lets metrics from different providers share a chart. The **range** selector windows every chart to a run
of releases. Below the charts, **compare two releases** lists
every metric side by side for any two releases, and the **binary composition** section shows where the text comes
from by module, provider service package and object-file section, plus the modules that moved most. Everything is
kept in the url hash so a view can be bookmarked or shared.

## Refreshing all data on one machine

Timing numbers (startup, schema, build, lint) only compare well when every release was measured on the same machine
doing nothing else, so the reference data set is produced by one dedicated box rather than the weekly workflow. On
that machine, after cloning or pulling:

```sh
make full          # every provider in .tfpp.yml, every release, every stage, no sampling, --force: many hours
make update        # the same, but only releases that have no results yet (what to run after each new release)
```

Both download the pinned terraform into `.cache/tools`, use the pinned golangci-lint from `.tools/bin` for providers
without a custom linter, keep macOS awake with `caffeinate`, and log to `.cache/full.log` / `.cache/update.log`.
`ARGS` passes extra flags, e.g. `make full ARGS="-p azuread"` or `make update ARGS="--since 2026-01-01"`. Go
toolchains for each release's `.go-version` are fetched automatically. Export a `GITHUB_TOKEN` (`gh auth token`)
to avoid the unauthenticated releases API rate limit. When it finishes, commit `data/` and push; the next Pages
run picks the new data up.

For a rough budget on an M1 Ultra: download, binary, startup, schema and source take a few seconds per release;
a clean build and a cold lint of azurerm take several minutes each, so a full run over ~100 releases is an
overnight job. The run is resumable: rerunning `make update` continues with whatever has no results yet, and
`--retry` redoes stages that failed.

## Hosting on GitHub Pages

`.github/workflows/pages.yaml` profiles new releases every Friday (or on demand) and publishes the report with
GitHub Pages. Results and the rendered site live on the `gh-pages` branch (`<provider>/results/*.json`,
`<provider>/index.html`, a landing page at `/`), so each run only profiles releases without results and only
downloads those releases' zips - the binaries never enter the repository. Enable Pages with "Source: GitHub
Actions" and the site appears at `https://<owner>.github.io/<repo>/`. Absolute timings from shared runners are
noisier than local ones; the trends still hold. `--results-dir` is what makes this work and can be used locally
too.

## Cache layout

```
.cache/<provider>/
  releases.json          release list from the github api
  binaries/<version>/    zips and extracted binaries per platform (~550MB per azurerm release)
  src/                   blobless partial clone, checked out at whichever tag was profiled last
  results/<version>.json everything recorded for that release
  schema/<version>/      terraform working dir, dev_overrides config, wrapper, gzipped schema json
  gocache/ golangci-cache/  isolated build and lint caches for the sampled stages
```

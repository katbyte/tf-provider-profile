# tfpp - terraform provider profiler

`tfpp` downloads every release of a terraform provider and records how it has changed over time, then renders an
interactive report. It was written to answer "how has the azurerm provider's binary size changed" and grew into
everything that can be measured for free from a release zip, the provider's git tag, and a short terraform run.

## What it measures

| stage      | source            | cost        | records |
|------------|-------------------|-------------|---------|
| `download` | releases.hashicorp.com | network | release zips for each `--platforms` entry, extracted into `.cache/<provider>/binaries/<version>/<platform>/` |
| `binary`   | the zip + binary  | <1s         | compressed and uncompressed size, go version, linked modules and their versions, build settings, object-file sections (text, rodata, pclntab, ...), function and package counts, and text bytes attributed to every module, provider service package and large package (via the pclntab, which survives stripping) |
| `startup`  | the native binary | ~1s         | time from exec to the go-plugin handshake line and peak RSS at that point, no terraform involved |
| `schema`   | terraform         | ~2s         | wall time of `terraform providers schema -json`, the provider's own peak RSS while serving it (terraform launches the provider through a `tfpp _wrap` shim), schema json size, and counts of resources, data sources, ephemeral/list resources, actions, functions, attributes, blocks, deprecated attributes and nesting depth |
| `source`   | git checkout of the tag | ~10s  | go lines/files (tests, vendor and code-only split), tree size, service packages, typed vs untyped resources and data sources, test functions, documented resources/data sources/ephemeral/list/actions/functions, go.mod direct/indirect deps, go.sum lines, go version, and since the previous release: commits, authors, files changed, lines added/removed, days elapsed, changelog entries by section |
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

Flags can also be set through `TFPP_*` environment variables (e.g. `TFPP_TERRAFORM=...`) or a `.tfpp` env-format
file in the working directory or home directory keyed by flag name, e.g. `terraform=.cache/tools/terraform/terraform`.

The schema stage uses whatever `terraform` is on PATH unless `--terraform` points elsewhere. Older terraform
versions do not include ephemeral/list resources, actions or functions in `providers schema -json`, so for those
counts use a current terraform; the version used is recorded with each result.

Other providers work too: `tfpp -p aws run`. The source stage's typed/untyped resource patterns are azurerm's, and
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

package stages

import (
	"cmp"
	"context"
	"debug/buildinfo"
	"debug/elf"
	"debug/gosym"
	"debug/macho"
	"debug/pe"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/katbyte/tf-provider-profile/lib/results"
)

// binaryStage dissects the release binary for each platform: sizes, build info, sections, and text attributed by
// module/package/service via the pclntab (release binaries are stripped, so the symbol table is useless but the
// Go runtime's function table survives).
func (r *Runner) binaryStage(ctx context.Context, res *results.Result) (string, error) {
	if res.Binary == nil {
		res.Binary = map[string]*results.BinaryResult{}
	}
	var parts []string
	for _, platform := range r.Opts.Platforms {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		br, err := r.analyseBinary(res.Version, platform)
		if err != nil {
			return "", fmt.Errorf("%s: %w", platform, err)
		}
		res.Binary[platform] = br
		parts = append(parts, fmt.Sprintf("%s zip %s bin %s %s", platform, humanBytes(br.ZipBytes), humanBytes(br.BinaryBytes), br.GoVersion))
	}
	return strings.Join(parts, "; "), nil
}

func (r *Runner) analyseBinary(version, platform string) (*results.BinaryResult, error) {
	bin, err := r.binaryPath(version, platform)
	if err != nil {
		return nil, err
	}
	zst, err := os.Stat(r.zipPath(version, platform))
	if err != nil {
		return nil, fmt.Errorf("zip missing (needed for compressed size): %w", err)
	}
	bst, err := os.Stat(bin)
	if err != nil {
		return nil, err
	}

	br := &results.BinaryResult{
		Platform:      platform,
		ZipBytes:      zst.Size(),
		BinaryBytes:   bst.Size(),
		Deps:          map[string]string{},
		Settings:      map[string]string{},
		Sections:      map[string]int64{},
		TextByModule:  map[string]int64{},
		TextByService: map[string]int64{},
		TextByPackage: map[string]int64{},
	}

	bi, err := buildinfo.ReadFile(bin)
	if err != nil {
		return nil, fmt.Errorf("reading build info: %w", err)
	}
	br.GoVersion = bi.GoVersion
	br.ModulePath = bi.Main.Path
	var depPaths []string
	for _, d := range bi.Deps {
		v := d.Version
		if d.Replace != nil {
			v = d.Replace.Version + " (replaces " + d.Version + ")"
		}
		br.Deps[d.Path] = v
		depPaths = append(depPaths, d.Path)
	}
	br.DepCount = len(bi.Deps)
	for _, s := range bi.Settings {
		br.Settings[s.Key] = s.Value
	}

	pcln, textAddr, err := readSections(bin, br.Sections)
	if err != nil {
		return nil, err
	}
	if pcln == nil {
		return br, nil // no pclntab (not a Go binary?): sizes only
	}

	lt := gosym.NewLineTable(pcln, textAddr)
	tab, err := gosym.NewTable(nil, lt)
	if err != nil {
		return nil, fmt.Errorf("parsing pclntab: %w", err)
	}

	// longest-prefix module matching: sort dep paths longest first
	slices.SortFunc(depPaths, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	modOf := func(pkg string) string {
		if pkg == "" {
			return "(unknown)"
		}
		if pkg == bi.Main.Path || strings.HasPrefix(pkg, bi.Main.Path+"/") {
			return bi.Main.Path
		}
		for _, d := range depPaths {
			if pkg == d || strings.HasPrefix(pkg, d+"/") {
				return d
			}
		}
		if first, _, _ := strings.Cut(pkg, "/"); !strings.Contains(first, ".") {
			return "std"
		}
		return "(unmatched)"
	}
	servicePrefix := bi.Main.Path + "/internal/services/"

	pkgs := map[string]int64{}
	for _, fn := range tab.Funcs {
		sz := int64(fn.End - fn.Entry) //nolint:gosec // G115: function sizes are far below int64 range
		if sz <= 0 {
			continue
		}
		pkg := fn.PackageName()
		br.TextAttributed += sz
		pkgs[pkg] += sz
		br.TextByModule[modOf(pkg)] += sz
		if rest, ok := strings.CutPrefix(pkg, servicePrefix); ok {
			svc, _, _ := strings.Cut(rest, "/")
			br.TextByService[svc] += sz
		}
	}
	br.Funcs = len(tab.Funcs)
	br.Packages = len(pkgs)

	// keep only packages big enough to matter in a per-release trend (>= 64KB) to bound the result size
	for pkg, sz := range pkgs {
		if sz >= 64*1024 {
			br.TextByPackage[pkg] = sz
		}
	}

	return br, nil
}

// readSections fills sizes for the well-known sections and returns the pclntab bytes and text start address.
func readSections(path string, sections map[string]int64) (pcln []byte, textAddr uint64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()

	if ef, err := elf.NewFile(f); err == nil {
		for _, s := range ef.Sections {
			if s.Type == elf.SHT_NULL {
				continue
			}
			sections[normaliseSection(s.Name)] += int64(s.Size) //nolint:gosec // G115: section sizes fit
			switch s.Name {
			case ".gopclntab":
				pcln, err = s.Data()
				if err != nil {
					return nil, 0, fmt.Errorf("reading .gopclntab: %w", err)
				}
			case ".text":
				textAddr = s.Addr
			}
		}
		// pclntab can also live inside .data.rel.ro on some builds; fall back to the runtime symbols if needed
		if pcln == nil {
			pcln = elfPclntabFromSymbols(ef)
		}
		return pcln, textAddr, nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, 0, err
	}

	if mf, err := macho.NewFile(f); err == nil {
		for _, s := range mf.Sections {
			sections[normaliseSection(s.Name)] += int64(s.Size) //nolint:gosec // G115: section sizes fit
			switch s.Name {
			case "__gopclntab":
				pcln, err = s.Data()
				if err != nil {
					return nil, 0, fmt.Errorf("reading __gopclntab: %w", err)
				}
			case "__text":
				if s.Seg == "__TEXT" {
					textAddr = s.Addr
				}
			}
		}
		return pcln, textAddr, nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, 0, err
	}

	if pf, err := pe.NewFile(f); err == nil {
		// windows: pclntab lives inside .rdata; locate via runtime.pclntab/epclntab symbols
		var start, end uint64
		for _, s := range pf.Sections {
			sections[normaliseSection(s.Name)] += int64(s.Size)
			if s.Name == ".text" {
				textAddr = imageBase(pf) + uint64(s.VirtualAddress)
			}
		}
		for _, s := range pf.Symbols {
			switch s.Name {
			case "runtime.pclntab":
				start = uint64(s.Value) + uint64(pf.Sections[s.SectionNumber-1].VirtualAddress)
			case "runtime.epclntab":
				end = uint64(s.Value) + uint64(pf.Sections[s.SectionNumber-1].VirtualAddress)
			}
		}
		if start == 0 || end <= start {
			return nil, textAddr, nil
		}
		for _, s := range pf.Sections {
			va := uint64(s.VirtualAddress)
			if va <= start && end <= va+uint64(s.VirtualSize) {
				data, err := s.Data()
				if err != nil {
					return nil, 0, err
				}
				sections["pclntab"] = int64(end - start)
				return data[start-va : end-va], textAddr, nil
			}
		}
		return nil, textAddr, nil
	}

	return nil, 0, errors.New("not an ELF, Mach-O or PE binary")
}

// elfPclntabFromSymbols locates the pclntab through the runtime.pclntab/epclntab symbols when there is no
// dedicated section for it.
func elfPclntabFromSymbols(ef *elf.File) []byte {
	syms, err := ef.Symbols()
	if err != nil {
		return nil
	}
	var start, end uint64
	for _, s := range syms {
		switch s.Name {
		case "runtime.pclntab":
			start = s.Value
		case "runtime.epclntab":
			end = s.Value
		}
	}
	if start == 0 || end <= start {
		return nil
	}
	for _, s := range ef.Sections {
		if s.Addr > start || end > s.Addr+s.Size {
			continue
		}
		data, err := s.Data()
		if err != nil {
			return nil
		}
		return data[start-s.Addr : end-s.Addr]
	}
	return nil
}

func imageBase(pf *pe.File) uint64 {
	switch oh := pf.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		return oh.ImageBase
	case *pe.OptionalHeader32:
		return uint64(oh.ImageBase)
	}
	return 0
}

// normaliseSection maps ELF/Mach-O/PE section names onto a common vocabulary so they chart across platforms.
func normaliseSection(name string) string {
	n := strings.TrimPrefix(name, ".")
	n = strings.TrimPrefix(n, "__")
	switch n {
	case "text", "rodata", "data", "bss", "noptrdata", "noptrbss", "typelink", "itablink", "symtab", "strtab":
		return n
	case "gopclntab":
		return "pclntab"
	case "go.buildinfo", "go_buildinfo":
		return "buildinfo"
	case "rdata":
		return "rodata"
	}
	if strings.HasPrefix(n, "debug") || strings.HasPrefix(n, "zdebug") {
		return "debug"
	}
	return "other"
}

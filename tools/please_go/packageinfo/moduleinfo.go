package packageinfo

import (
	"encoding/json"
	"fmt"
	"go/build"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// WriteModuleInfo writes package info to the given writer for a module.
func WriteModuleInfo(
	modulePath string,
	srcRoot string,
	importconfig string,
	installPkgs []string,
	w io.Writer,
) error {

	goFiles := map[string][]string{}

	if len(installPkgs) == 0 {
		// install everything
		installPkgs = []string{"..."}
	}

	for _, pkg := range installPkgs {
		if strings.Contains(pkg, "...") {
			pkg = strings.TrimSuffix(pkg, "...")
			if err := filepath.WalkDir(filepath.Join(srcRoot, pkg), walkDirFunc(goFiles, false)); err != nil {
				return fmt.Errorf("failed to read module dir: %w", err)
			}
		} else {
			dir := filepath.Join(srcRoot, pkg)
			goFiles[dir] = append(goFiles[dir], filepath.Join(srcRoot, pkg))
		}
	}

	var pkgs []*packages.Package
	for dir := range goFiles {
		pkgDir := strings.TrimPrefix(strings.TrimPrefix(dir, srcRoot), "/")
		bpkg, err := buildPackage(filepath.Join(modulePath, pkgDir), dir)
		if _, ok := err.(*build.NoGoError); ok {
			continue // Don't really care, this happens sometimes for modules
		} else if err != nil {
			return fmt.Errorf("failed to import directory %s: %w", dir, err)
		}

		pkgs = append(pkgs, FromModuleBuildPackage(bpkg))
	}

	imports, err := loadImportConfig(importconfig)
	if err != nil {
		return fmt.Errorf("failed to read importconfig: %w", err)
	}

	for _, pkg := range pkgs {
		pkg.ExportFile = imports[pkg.PkgPath]
	}

	pkgs = slices.DeleteFunc(pkgs, func(pkg *packages.Package) bool {
		_, present := imports[pkg.PkgPath]
		return !present
	})

	// Vendor packages. They aren't identified by the original imports but we know what they are now.
	vendorised := map[string]*packages.Package{}
	for _, pkg := range pkgs {
		if after, ok := strings.CutPrefix(pkg.PkgPath, "vendor/"); ok {
			vendorised[after] = pkg
		}
	}
	for _, pkg := range pkgs {
		for k := range pkg.Imports {
			if v, present := vendorised[k]; present {
				pkg.Imports[k] = v
			}
		}
	}

	// Ensure output is deterministic
	sort.Slice(pkgs, func(i, j int) bool {
		return pkgs[i].ID < pkgs[j].ID
	})
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(pkgs)
}

// FromModuleBuildPackage creates a [packages.Package] from a [build.Package] for a module.
func FromModuleBuildPackage(
	bpkg *build.Package,
) *packages.Package {
	goFiles := make([]string, len(bpkg.GoFiles))
	compiledGoFiles := make([]string, len(bpkg.GoFiles))

	for i, file := range bpkg.GoFiles {
		goFiles[i] = filepath.Join(bpkg.Dir, file)
		compiledGoFiles[i] = filepath.Join(bpkg.Dir, file)
	}

	imports := make(map[string]*packages.Package, len(bpkg.Imports))
	for _, imp := range bpkg.Imports {
		imports[imp] = &packages.Package{ID: imp, PkgPath: imp}
	}

	pkg := &packages.Package{
		ID:              bpkg.ImportPath,
		Name:            bpkg.Name,
		PkgPath:         bpkg.ImportPath,
		GoFiles:         goFiles,
		CompiledGoFiles: compiledGoFiles,
		OtherFiles:      slices.Concat(bpkg.CFiles, bpkg.CXXFiles, bpkg.MFiles, bpkg.HFiles, bpkg.SFiles, bpkg.SwigFiles, bpkg.SwigCXXFiles, bpkg.SysoFiles),
		EmbedPatterns:   bpkg.EmbedPatterns,
		Imports:         imports,
	}
	return pkg
}

// loadImportConfig reads the given importconfig file and produces a map of package name -> export path
func loadImportConfig(filename string) (map[string]string, error) {
	b, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(b), "\n")
	m := make(map[string]string, len(lines))
	for _, line := range lines {
		if after, ok := strings.CutPrefix(line, "packagefile "); ok {
			pkg, exportFile, found := strings.Cut(after, "=")
			if !found {
				return nil, fmt.Errorf("unknown syntax for line: %s", line)
			}
			m[pkg] = exportFile
		}
	}
	return m, nil
}

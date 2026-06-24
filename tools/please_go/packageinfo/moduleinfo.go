package packageinfo

import (
	"encoding/json"
	"fmt"
	"go/build"
	"io"
	"io/fs"
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
	target string,
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
			if err := filepath.WalkDir(filepath.Join(srcRoot, pkg), func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				} else if name := d.Name(); name == "testdata" || strings.HasPrefix(name, "_") || d.Name() == "cmd" {
					return filepath.SkipDir // Don't descend into testdata
				} else if strings.HasSuffix(name, ".go") {
					dir := filepath.Dir(path)
					goFiles[dir] = append(goFiles[dir], path)
				}
				return nil
			}); err != nil {
				return fmt.Errorf("failed to read module dir: %w", err)
			}
		} else {
			dir := filepath.Join(srcRoot, pkg)
			goFiles[dir] = append(goFiles[dir], filepath.Join(srcRoot, pkg))
		}
	}

	var pkgs []*packages.Package
	exportFileByPkgPath, err := loadImportConfig(importconfig)
	if err != nil {
		return fmt.Errorf("failed to read importconfig: %w", err)
	}
	for dir := range goFiles {
		pkgDir := strings.TrimPrefix(strings.TrimPrefix(dir, srcRoot), "/")
		bpkg, err := buildPackage(filepath.Join(modulePath, pkgDir), dir)
		if _, ok := err.(*build.NoGoError); ok {
			continue // Don't really care, this happens sometimes for modules
		} else if err != nil {
			return fmt.Errorf("failed to import directory %s: %w", dir, err)
		}

		pkgs = append(pkgs, FromModuleBuildPackage(bpkg, target, pkgDir, exportFileByPkgPath))
	}

	// In the stdlib source code (and therefore in the imports reported by build.ImportDir) vendored packages
	// are imported using the non-vendored path. However, the vendored paths are used in the importconfig +
	// exportFile (which is used by go/packages). The packages returned must therefore have their PkgPath and
	// Imports aligned with the exportFile. After loading all of the packages we can see which packages are vendored
	// and replace any imports pointing to the unvendored paths to the vendored paths.
	isVendored := make(map[string]bool)
	for _, pkg := range pkgs {
		if pkgPath, ok := strings.CutPrefix(pkg.PkgPath, "vendor/"); ok {
			isVendored[pkgPath] = true
		}
	}
	for _, pkg := range pkgs {
		newImports := make(map[string]*packages.Package, len(pkg.Imports))
		for imp := range pkg.Imports {
			if isVendored[imp] {
				newImports["vendor/"+imp] = &packages.Package{}
			} else {
				newImports[imp] = &packages.Package{}
			}
		}
		pkg.Imports = newImports
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
	target string,
	pkgDir string,
	exportFileByPackagePath map[string]string,
) *packages.Package {
	goFiles := make([]string, len(bpkg.GoFiles))
	compiledGoFiles := make([]string, len(bpkg.GoFiles))

	for i, file := range bpkg.GoFiles {
		goFiles[i] = filepath.Join(bpkg.Dir, file)
		compiledGoFiles[i] = filepath.Join(bpkg.Dir, file)
	}

	imports := make(map[string]*packages.Package, len(bpkg.Imports))
	for _, imp := range bpkg.Imports {
		if imp == "C" {
			continue
		}
		imports[imp] = &packages.Package{}
	}

	pkg := &packages.Package{
		ID:              target + " " + pkgDir,
		Name:            bpkg.Name,
		PkgPath:         bpkg.ImportPath,
		GoFiles:         goFiles,
		CompiledGoFiles: compiledGoFiles,
		OtherFiles:      slices.Concat(bpkg.CFiles, bpkg.CXXFiles, bpkg.MFiles, bpkg.HFiles, bpkg.SFiles, bpkg.SwigFiles, bpkg.SwigCXXFiles, bpkg.SysoFiles),
		EmbedPatterns:   bpkg.EmbedPatterns,
		Imports:         imports,
		ExportFile:      exportFileByPackagePath[bpkg.ImportPath],
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

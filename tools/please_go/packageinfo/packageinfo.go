// Package packageinfo writes information about Go packages in a JSON format.
// This is created at build time and intended to be consumed by the gopackagedriver binary.
package packageinfo

import (
	"encoding/json"
	"fmt"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

// WritePackageInfo writes a series of package info files to the given file.
func WritePackageInfo(
	importPath string,
	srcRoot string,
	exportFile string,
	subrepo string,
	module string,
	target string,
	w io.Writer,
) error {

	b, err := os.ReadFile("_plz/named_srcs/srcs")
	if err != nil {
		return err
	}

	module = modulePath(module, importPath)
	bpkgByDir := make(map[string]*build.Package)
	for _, src := range strings.Fields(string(b)) {
		dir := filepath.Dir(src)
		if _, ok := bpkgByDir[dir]; ok {
			continue
		}

		bpkg, err := buildPackage(importPath, dir)
		if _, ok := err.(*build.NoGoError); ok {
			// This can happen if, for example, the package only contains files with
			// leading underscores. Ignoring these is consistent with go list behaviour.
			continue
		} else if err != nil {
			return fmt.Errorf("failed to import directory %s: %w", dir, err)
		}
		bpkgByDir[dir] = bpkg

	}
	pkg, err := fromBuildPackage(bpkgByDir, subrepo, module, target, exportFile)
	if err != nil {
		return fmt.Errorf("building packages.Package from build.Package: %w", err)
	}
	pkgs := []*packages.Package{pkg}

	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(pkgs)
}

func walkDirFunc(goFiles map[string][]string) func(string, fs.DirEntry, error) error {
	return func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		} else if name := d.Name(); strings.HasSuffix(name, ".go") {
			dir := filepath.Dir(path)
			goFiles[dir] = append(goFiles[dir], path)
		}
		return nil
	}
}

func buildPackage(
	pkgPath string,
	pkgDir string,
) (*build.Package, error) {
	if pkgDir == "" || pkgDir == "." {
		// This happens when we're in the repo root, ImportDir refuses to read it for some reason.
		path, err := filepath.Abs(pkgDir)
		if err != nil {
			return nil, err
		}
		pkgDir = path
	}
	bpkg, err := build.ImportDir(pkgDir, build.ImportComment)
	if err != nil {
		return nil, err
	}
	bpkg.ImportPath = pkgPath
	return bpkg, nil
}

func importsFromFile(filePath string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	var imports []string
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, err
		}
		imports = append(imports, path)
	}
	return imports, nil
}

// fromBuildPackage creates a [packages.Package] from a [build.Package].
func fromBuildPackage(
	bpkgByDir map[string]*build.Package,
	subrepo string,
	module string,
	target string,
	exportFile string,
) (*packages.Package, error) {

	var name string
	var importPath string
	for _, bpkg := range bpkgByDir {
		// We don't rely on bpkg.Name as it doesn't account for whether the directory contains an external test package.
		// Attempting to infer whether the pacakge name is actually '{name}_test' requires relying on go naming
		// conventions that please does not enforce.
		var err error
		name, err = packageName(filepath.Join(bpkg.Dir, slices.Concat(bpkg.GoFiles, bpkg.TestGoFiles, bpkg.XTestGoFiles, bpkg.CgoFiles)[0]))
		if err != nil {
			return nil, fmt.Errorf("getting package name from first go file: %w", err)
		}
		importPath = bpkg.ImportPath
		break
	}

	var compiledGoFiles []string
	var goFiles []string
	var otherFiles []string
	var embedPatterns []string
	for _, bpkg := range bpkgByDir {
		// We don't rely on the way that build.ImportDir categorises the files as these rely on specific go
		// naming conventions that please doesn't enforce. Instead, we rely on all the sources present in the
		// build sandbox being the ones that we need to construct the package.

		for _, file := range slices.Concat(bpkg.GoFiles, bpkg.TestGoFiles, bpkg.XTestGoFiles) {
			path := filepath.Join(bpkg.Dir, file)
			compiledGoFiles = append(compiledGoFiles, path)
		}
		for _, file := range slices.Concat(bpkg.GoFiles, bpkg.TestGoFiles, bpkg.XTestGoFiles, bpkg.CgoFiles) {
			if strings.HasSuffix(file, ".cgo1.go") {
				continue
			}
			var path string
			if subrepo != "" {
				// this is fairly nasty... there must be a better way of getting it without the pkg/ prefix
				dir := strings.TrimPrefix(bpkg.Dir, "pkg/"+runtime.GOOS+"_"+runtime.GOARCH)
				dir = strings.TrimPrefix(strings.TrimPrefix(dir, "/"), module)
				path = filepath.Join(subrepo, dir, file)
			} else {
				path = filepath.Join(bpkg.Dir, file)
			}
			goFiles = append(goFiles, path)
		}
		otherFiles = append(otherFiles, slices.Concat(bpkg.CFiles, bpkg.CXXFiles, bpkg.MFiles, bpkg.HFiles, bpkg.SFiles, bpkg.SwigFiles, bpkg.SwigCXXFiles, bpkg.SysoFiles)...)
		embedPatterns = append(embedPatterns, bpkg.EmbedPatterns...)
	}

	imports := make(map[string]*packages.Package)
	for _, bpkg := range bpkgByDir {
		for _, imp := range slices.Concat(bpkg.Imports, bpkg.TestImports, bpkg.XTestImports) {
			if imp == "C" {
				continue
			}
			imports[imp] = &packages.Package{}
		}
	}

	for _, bpkg := range bpkgByDir {
		cgoTypes := filepath.Join(bpkg.Dir, "_cgo_gotypes.go")
		if _, err := os.Stat(cgoTypes); err == nil {
			compiledGoFiles = append(compiledGoFiles, cgoTypes)
			cgoTypesImports, err := importsFromFile(cgoTypes)
			if err != nil {
				return nil, fmt.Errorf("adding cgo types imports to packages.Package: %w", err)
			}
			for _, imp := range cgoTypesImports {
				imports[imp] = &packages.Package{}
			}
		}
	}

	if subrepo != "" {
		// The export file in plz-out/gen is located at {subrepo}/{relative package path}/{import_file}.a
		// We can get the relative package path by trimming the module prefix.
		relPath := strings.TrimPrefix(importPath, module)
		relPath = strings.TrimPrefix(relPath, "/")

		// This is a really gross hack to sneak both paths through the one field.
		exportFile = filepath.Join(subrepo, relPath, filepath.Base(exportFile)) + "|" + exportFile
	}

	pkg := &packages.Package{
		ID:              target,
		Name:            name,
		PkgPath:         importPath,
		GoFiles:         goFiles,
		CompiledGoFiles: compiledGoFiles,
		OtherFiles:      otherFiles,
		EmbedPatterns:   embedPatterns,
		Imports:         imports,
		ExportFile:      exportFile,
	}

	return pkg, nil
}

// modulePath returns the import path for a module, or the given one if the module isn't set.
func modulePath(module, importPath string) string {
	if module == "" {
		return importPath
	}
	before, _, _ := strings.Cut(module, "@")
	return before
}

func packageName(filePath string) (string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, nil, parser.PackageClauseOnly)
	if err != nil {
		return "", err
	}
	return f.Name.Name, nil
}

package main

import (
	"archive/zip"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
)

var (
	verbose = flag.Bool("v", false, "print info on each step as it happens")
	zipPath = flag.String("zip", "", "path of the zip file containing the fonts")
	zipDir  = flag.String("zipdir", "", "only process files that match this path prefix within the zip")
)

func logInfo(format string, args ...any) {
	if *verbose {
		fmt.Printf(format, args...)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(2)
}

// baseNameStem returns the file name of the given file path without its extension. For
// example, "test.txt" would return "txt".
func baseNameStem(s string) string {
	if idx := strings.LastIndexByte(s, '.'); idx >= 0 {
		return s[:idx]
	}
	return s
}

// copyToDisk copies the given Reader to the file at the given disk path, creating that
// file if it doesn't exist or truncating it if it does.
func copyToDisk(in io.Reader, diskPath string) error {
	out, err := os.OpenFile(diskPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

var (
	// This is the template for each font variant's single Go source file which embeds and
	// exports the corresponding OTF (or TTF) file content as a byte slice.
	//
	//go:embed variant_pkg.go.tmpl
	variantPkgCodeTmplStr string
	variantPkgCodeTmpl    = template.Must(template.New("variantPkgCode").Parse(variantPkgCodeTmplStr))

	// This is the template for a font's root package which parses and registers all of the
	// exported OTF (or TTF) variants from its sub packages in a collection of Gio font faces.
	//
	//go:embed root_pkg.go.tmpl
	rootPkgCodeTmplStr string
	rootPkgCodeTmpl    = template.Must(template.New("rootPkgCode").Parse(rootPkgCodeTmplStr))

	// This is the template for a font's README file that goes in the root of the
	// generated directory.
	//
	//go:embed readme.md.tmpl
	readmeTmplStr string
	readmeTmpl    = template.Must(template.New("readme").Parse(readmeTmplStr))
)

type fontPkgInfo struct {
	PkgName     string
	DirName     string
	ModPath     string
	Variants    []variantPkgInfo
	LicenseFile string
}

type variantPkgInfo struct {
	FontFileName string // The source file (ex: "Vegur-Bold.otf")
	PkgName      string // Derived from the source file name (ex: "vegurbold")
	DataVarName  string // The all-caps file extension of the source file (ex: "OTF" or "TTF")
}

func createVariantPkg(fnt *fontPkgInfo, f *zip.File) error {
	fname := f.FileInfo().Name()
	variantPkgName := baseNameStem(fname)
	variantPkgName = strings.ToLower(strings.Replace(variantPkgName, "-", "", -1))

	variantDir := fnt.DirName + "/" + variantPkgName
	if err := os.Mkdir(variantDir, 0o755); err != nil {
		if os.IsExist(err) {
			logInfo("directory '%s' already exists\n", variantDir)
		} else {
			return err
		}
	}

	inFile, err := f.Open()
	if err != nil {
		return fmt.Errorf("opening in-file '%s': %v", fname, err)
	}
	defer inFile.Close()

	if err = copyToDisk(inFile, variantDir+"/"+fname); err != nil {
		return fmt.Errorf("copying font variant file: %w", err)
	}

	// In each font variant Go package, there's a source file named 'data.go' that embeds
	// and exports its corresponding OTF (or TTF) file content as a byte slice.
	outGoPath := variantDir + "/data.go"
	outGoFile, err := os.OpenFile(outGoPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer outGoFile.Close()

	variant := variantPkgInfo{
		PkgName:      variantPkgName,
		FontFileName: fname,
		DataVarName:  strings.ToUpper(filepath.Ext(fname)[1:]),
	}

	if err = variantPkgCodeTmpl.Execute(outGoFile, &variant); err != nil {
		return err
	}

	fnt.Variants = append(fnt.Variants, variant)
	return nil
}

func writePkgRootFile(fnt *fontPkgInfo) error {
	f, err := os.OpenFile(fnt.PkgName+".go", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	if err = rootPkgCodeTmpl.Execute(f, fnt); err != nil {
		return err
	}
	return nil
}

func writeModFile(fnt *fontPkgInfo) error {
	if err := exec.Command("go", "mod", "init", fnt.ModPath).Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return fmt.Errorf("running go mod init: %w", err)
		}
	}
	if err := exec.Command("go", "mod", "tidy").Run(); err != nil {
		return fmt.Errorf("running go mod tidy: %w", err)
	}
	return nil
}

func copyLicenseFile(fnt *fontPkgInfo, f *zip.File) error {
	lf, err := f.Open()
	if err != nil {
		return fmt.Errorf("opening license zip file: %w", err)
	}
	defer lf.Close()

	if err = copyToDisk(lf, fnt.DirName+"/"+f.Name); err != nil {
		return err
	}

	fnt.LicenseFile = f.Name
	return nil
}

func isLicenseFile(fname string) bool {
	return strings.ToLower(baseNameStem(fname)) == "ofl"
}

func writeReadme(fnt *fontPkgInfo) error {
	f, err := os.OpenFile("README.md", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	if err = readmeTmpl.Execute(f, fnt); err != nil {
		return err
	}
	return nil
}

func initGitAndStageDiff(fnt *fontPkgInfo) error {
	if _, err := os.Stat(".git"); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if err := exec.Command("git", "init").Run(); err != nil {
			return fmt.Errorf("running 'git init': %w", err)
		}
		origin := "git@github.com:gio-tools/font-" + fnt.PkgName + ".git"
		if err := exec.Command("git", "remote", "add", "origin", origin).Run(); err != nil {
			return fmt.Errorf("running 'git remote add origin': %w", err)
		}
	}
	if err := exec.Command("git", "add", "-A").Run(); err != nil {
		return fmt.Errorf("running 'git add -A': %w", err)
	}
	return nil
}

func main() {
	flag.Parse()

	zipName := filepath.Base(*zipPath)
	pkgName := strings.ToLower(baseNameStem(zipName))
	pkgName = strings.Replace(pkgName, "-", "", -1)

	fnt := fontPkgInfo{
		PkgName: pkgName,
		ModPath: "gio.tools/fonts/" + pkgName,
		DirName: "font-" + pkgName,
	}

	logInfo("font name '%s'\n", fnt.PkgName)

	z, err := zip.OpenReader(*zipPath)
	if err != nil {
		fatalf("opening zip file: %v", err)
	}
	defer z.Close()

	// Make the parent output directory.
	if err = os.Mkdir(fnt.DirName, 0o755); err != nil {
		if os.IsExist(err) {
			logInfo("target output directory '%s' already exists\n", fnt.PkgName)
		} else {
			fatalf("%v", err)
		}
	}

	for _, f := range z.File {
		if !strings.HasPrefix(f.Name, *zipDir) {
			continue
		}
		ext := filepath.Ext(f.Name)
		if ext != "" {
			ext = ext[1:]
		}
		switch ext {
		// The only text file of interest at this point would be a license file.
		case "txt":
			if isLicenseFile(f.Name) {
				if err = copyLicenseFile(&fnt, f); err != nil {
					fatalf("copying license file: %v", err)
				}
			}
		// Create a sub-package for each font variant.
		case "otf", "ttf":
			err := createVariantPkg(&fnt, f)
			if err != nil {
				fatalf("creating font variant pkg: %v", err)
			}
		default:
			logInfo("skipping file '%s'\n", f.Name)
		}
	}

	sort.SliceStable(fnt.Variants, func(i, j int) bool {
		return fnt.Variants[i].PkgName < fnt.Variants[j].PkgName
	})

	if err = os.Chdir(fnt.DirName); err != nil {
		fatalf("cd-ing into font dir: %w", err)
	}

	if err = writePkgRootFile(&fnt); err != nil {
		fatalf("writing pkg root file: %v", err)
	}

	if err = writeModFile(&fnt); err != nil {
		fatalf("%v", err)
	}

	if err = writeReadme(&fnt); err != nil {
		fatalf("writing readme: %v", err)
	}

	if err = initGitAndStageDiff(&fnt); err != nil {
		fatalf("%v", err)
	}

	// Make sure there's a file in the website for this font's vanity module path.
	err = os.WriteFile("../website/content/fonts/"+fnt.PkgName+".md", []byte{}, 0o644)
	if err != nil {
		fatalf("making vanity path entry in website: %v", err)
	}
}

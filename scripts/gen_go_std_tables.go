// Copyright (c) 2024, The Garble Authors.
// See LICENSE for licensing information.

//go:build ignore

// This is a program used with `go generate`, so it handles errors via panic.
package main

import (
	"bytes"
	"cmp"
	"fmt"
	"go/format"
	"go/version"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"
)

var goVersions = []string{"go1.24.1"}

var tmplTables = template.Must(template.New("").Parse(`
// Code generated by scripts/gen_go_std_tables.go; DO NOT EDIT.

// Generated from Go versions {{ .GoVersions }}.

package main

var runtimeAndDeps = map[string]bool{
{{- range $path := .RuntimeAndDeps }}
	"{{ $path.String }}": true, // {{ $path.GoVersionLang }}
{{- end }}
}

var runtimeLinknamed = []string{
{{- range $path := .RuntimeLinknamed }}
	"{{ $path.String }}", // {{ $path.GoVersionLang }}
{{- end }}
	// The net package linknames to the runtime, not the other way around.
	// TODO: support this automatically via our script.
	"net",
}

var compilerIntrinsics = map[string]map[string]bool{
{{- range $intr := .CompilerIntrinsics }}
	"{{ $intr.Path }}": {
{{- range $name := $intr.Names }}
		"{{ $name.String }}": true, // {{ $name.GoVersionLang }}
{{- end }}
	},
{{- end }}
}

var reflectSkipPkg = map[string]bool{
	"fmt": true,
}
`[1:]))

type tmplData struct {
	GoVersions         []string
	RuntimeAndDeps     []versionedString
	RuntimeLinknamed   []versionedString
	CompilerIntrinsics []tmplIntrinsic
}

type tmplIntrinsic struct {
	Path  string
	Names []versionedString
}

func (t tmplIntrinsic) Compare(t2 tmplIntrinsic) int {
	return cmp.Compare(t.Path, t2.Path)
}

func (t tmplIntrinsic) Equal(t2 tmplIntrinsic) bool {
	return t.Compare(t2) == 0
}

type versionedString struct {
	String        string
	GoVersionLang string
}

func (v versionedString) Compare(v2 versionedString) int {
	if c := cmp.Compare(v.String, v2.String); c != 0 {
		return c
	}
	// Negated so that newer Go versions go first.
	return -cmp.Compare(v.GoVersionLang, v2.GoVersionLang)
}

func (v versionedString) Equal(v2 versionedString) bool {
	// Note that we do equality based on String alone,
	// because we only need one String entry with the latest version.
	return v.String == v2.String
}

func cmdGo(goVersion string, args ...string) versionedString {
	cmd := exec.Command("go", args...)
	cmd.Env = append(cmd.Environ(), "GOTOOLCHAIN="+goVersion)
	out, err := cmd.Output()
	if err != nil {
		panic(err)
	}
	return versionedString{
		String:        string(bytes.TrimSpace(out)), // no trailing newline
		GoVersionLang: version.Lang(goVersion),
	}
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func lines(vs versionedString) []versionedString {
	split := strings.Split(vs.String, "\n")
	var versioned []versionedString
	for _, s := range split {
		versioned = append(versioned, versionedString{
			String:        s,
			GoVersionLang: vs.GoVersionLang,
		})
	}
	return versioned
}

var rxLinkname = regexp.MustCompile(`^//go:linkname .* ([^.]*)\.[^.]*$`)
var rxIntrinsic = regexp.MustCompile(`\b(addF|alias)\("([^"]*)", "([^"]*)",`)

func main() {
	var runtimeAndDeps []versionedString
	for _, goVersion := range goVersions {
		runtimeAndDeps = append(runtimeAndDeps, lines(cmdGo(goVersion, "list", "-deps", "runtime"))...)
	}
	slices.SortFunc(runtimeAndDeps, versionedString.Compare)
	runtimeAndDeps = slices.CompactFunc(runtimeAndDeps, versionedString.Equal)

	var goroots []versionedString
	for _, goVersion := range goVersions {
		goroots = append(goroots, cmdGo(goVersion, "env", "GOROOT"))
	}

	// All packages that the runtime linknames to, except runtime and its dependencies.
	// This resulting list is what we need to "go list" when obfuscating the runtime,
	// as they are the packages that we may be missing.
	var runtimeLinknamed []versionedString
	for _, goroot := range goroots {
		runtimeGoFiles, err := filepath.Glob(filepath.Join(goroot.String, "src", "runtime", "*.go"))
		if err != nil {
			panic(err)
		}
		for _, goFile := range runtimeGoFiles {
			for line := range strings.SplitSeq(readFile(goFile), "\n") {
				m := rxLinkname.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				path := m[1]
				switch path {
				case "main", "runtime/metrics_test":
					continue
				}
				runtimeLinknamed = append(runtimeLinknamed, versionedString{
					String:        path,
					GoVersionLang: goroot.GoVersionLang,
				})
			}
		}
	}
	slices.SortFunc(runtimeLinknamed, versionedString.Compare)
	runtimeLinknamed = slices.CompactFunc(runtimeLinknamed, versionedString.Equal)
	runtimeLinknamed = slices.DeleteFunc(runtimeLinknamed, func(path versionedString) bool {
		for _, prev := range runtimeAndDeps {
			if prev.String == path.String {
				return true
			}
		}
		return false
	})

	compilerIntrinsicsIndexByPath := make(map[string]int)
	var compilerIntrinsics []tmplIntrinsic
	for _, goroot := range goroots {
		for line := range strings.SplitSeq(readFile(filepath.Join(
			goroot.String, "src", "cmd", "compile", "internal", "ssagen", "intrinsics.go",
		)), "\n") {
			m := rxIntrinsic.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			path, name := m[2], m[3]
			vs := versionedString{
				String:        name,
				GoVersionLang: goroot.GoVersionLang,
			}
			if i := compilerIntrinsicsIndexByPath[path]; i == 0 {
				compilerIntrinsicsIndexByPath[path] = len(compilerIntrinsics)
				compilerIntrinsics = append(compilerIntrinsics, tmplIntrinsic{
					Path:  path,
					Names: []versionedString{vs},
				})
			} else {
				compilerIntrinsics[i].Names = append(compilerIntrinsics[i].Names, vs)
			}
		}
	}
	slices.SortFunc(compilerIntrinsics, tmplIntrinsic.Compare)
	compilerIntrinsics = slices.CompactFunc(compilerIntrinsics, tmplIntrinsic.Equal)
	for path := range compilerIntrinsics {
		intr := &compilerIntrinsics[path]
		slices.SortFunc(intr.Names, versionedString.Compare)
		intr.Names = slices.CompactFunc(intr.Names, versionedString.Equal)
	}

	var buf bytes.Buffer
	if err := tmplTables.Execute(&buf, tmplData{
		GoVersions:         goVersions,
		RuntimeAndDeps:     runtimeAndDeps,
		RuntimeLinknamed:   runtimeLinknamed,
		CompilerIntrinsics: compilerIntrinsics,
	}); err != nil {
		panic(err)
	}
	out := buf.Bytes()
	formatted, err := format.Source(out)
	if err != nil {
		fmt.Println(string(out))
		panic(err)
	}

	if err := os.WriteFile("go_std_tables.go", formatted, 0o666); err != nil {
		panic(err)
	}
}

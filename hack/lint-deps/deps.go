// lint-deps is the dependency-policy gate (AGENTS.md, "Dependencies and
// versions"). Direct modules and imports are limited to this repository, the
// Go standard library, and what is hosted at k8s.io or one of its subdomains.
// Anything else needs an approved exception, listed in exceptions.json next to
// this file, and so does a package of an allowed host that wraps a module
// which is not allowed.
//
// The gate reads the imports of every Go file, whatever its build tags, and
// the direct requirements of go.mod. It only uses the standard library.
package main

import (
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	goModPath      = "go.mod"
	exceptionsPath = "hack/lint-deps/exceptions.json"

	allowedHost = "k8s.io"
	dateLayout  = time.DateOnly
)

// The uses that an exception can permit.
const (
	production = "production"
	tests      = "tests"
	tools      = "tools"
)

// allowlist is the content of exceptions.json.
type allowlist struct {
	// Wrappers are the packages of an allowed host that wrap a module which
	// is not allowed: importing them needs an exception.
	Wrappers []string `json:"wrappers"`
	// Exceptions are the approved exceptions.
	Exceptions []*exception `json:"exceptions"`
}

// exception is one approved exception. Imports and Wrappers hold import paths;
// a path ending with "/..." also covers the packages below it.
type exception struct {
	Name string `json:"name"`
	// Modules are the modules that go.mod may require directly.
	Modules []string `json:"modules"`
	// Imports are the packages that may be imported.
	Imports []string `json:"imports"`
	// Uses says where: production, tests, tools.
	Uses []string `json:"uses"`
	// Reason says why the standard library and the Kubernetes modules are insufficient.
	Reason string `json:"reason"`
	// Approval refers to the maintainer approval.
	Approval string `json:"approval"`
	// Reviewed and NextReview are dates (YYYY-MM-DD).
	Reviewed   string `json:"reviewed"`
	NextReview string `json:"nextReview"`

	used bool
}

// lint returns the problems found in the repository: an empty result means
// that it follows the dependency policy.
func lint(repo fs.FS, today time.Time) ([]string, error) {
	listText, err := fs.ReadFile(repo, exceptionsPath)
	if err != nil {
		return nil, err
	}
	var list allowlist
	if err := json.Unmarshal(listText, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", exceptionsPath, err)
	}
	goMod, err := fs.ReadFile(repo, goModPath)
	if err != nil {
		return nil, err
	}
	module, required := parseGoMod(string(goMod))

	var problems []string
	for _, exception := range list.Exceptions {
		problems = append(problems, exception.check(today)...)
	}
	for _, requirement := range required {
		if !hostAllowed(requirement) && !list.permitsModule(requirement) {
			problems = append(problems, fmt.Sprintf("%s: module %q is not hosted at %s and has no exception in %s",
				goModPath, requirement, allowedHost, exceptionsPath))
		}
	}
	importProblems, err := list.checkImports(repo, module)
	if err != nil {
		return nil, err
	}
	problems = append(problems, importProblems...)
	for _, exception := range list.Exceptions {
		if !exception.used {
			problems = append(problems, fmt.Sprintf("%s: exception %q: nothing imports it anymore: remove it", exceptionsPath, exception.Name))
		}
	}
	return problems, nil
}

// check validates the record of an exception, and its review dates: a review
// is due at most one year after the previous one, and must not be overdue.
func (e *exception) check(today time.Time) []string {
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf("%s: exception %q: %s", exceptionsPath, e.Name, fmt.Sprintf(format, args...)))
	}
	for _, use := range e.Uses {
		if use != production && use != tests && use != tools {
			report("unknown use %q, want %s, %s or %s", use, production, tests, tools)
		}
	}
	if e.Reason == "" {
		report("no reason")
	}
	if e.Approval == "" {
		report("no approval")
	}
	reviewed, err := time.Parse(dateLayout, e.Reviewed)
	if err != nil {
		report("reviewed %q is not a date (YYYY-MM-DD)", e.Reviewed)
		return problems
	}
	nextReview, err := time.Parse(dateLayout, e.NextReview)
	if err != nil {
		report("nextReview %q is not a date (YYYY-MM-DD)", e.NextReview)
		return problems
	}
	if nextReview.After(reviewed.AddDate(1, 0, 0)) {
		report("its next review (%s) is more than one year after the previous one (%s)", e.NextReview, e.Reviewed)
	}
	if !today.Before(nextReview) {
		report("its review was due on %s", e.NextReview)
	}
	return problems
}

// parseGoMod returns the module path and the modules that go.mod requires
// directly, which are the requirements without an "// indirect" comment.
func parseGoMod(text string) (module string, required []string) {
	inRequireBlock := false
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
		case fields[0] == "module" && len(fields) > 1:
			module = fields[1]
		case fields[0] == "require" && len(fields) > 1 && fields[1] == "(":
			inRequireBlock = true
		case fields[0] == ")":
			inRequireBlock = false
		case strings.Contains(line, "// indirect"):
		case fields[0] == "require" && len(fields) > 2:
			required = append(required, fields[1])
		case inRequireBlock && len(fields) > 1:
			required = append(required, fields[0])
		}
	}
	return module, required
}

// hostAllowed says whether an import or module path is hosted at k8s.io or one
// of its subdomains. It compares the host, not a part of it: k8s.io.example.org
// is not allowed.
func hostAllowed(importPath string) bool {
	host, _, _ := strings.Cut(importPath, "/")
	return host == allowedHost || strings.HasSuffix(host, "."+allowedHost)
}

// covers says whether a pattern covers an import path: the same path, or a
// path below a pattern ending with "/...".
func covers(pattern, importPath string) bool {
	if tree, isTree := strings.CutSuffix(pattern, "/..."); isTree {
		return importPath == tree || strings.HasPrefix(importPath, tree+"/")
	}
	return pattern == importPath
}

func coveredBy(patterns []string, importPath string) bool {
	return slices.ContainsFunc(patterns, func(pattern string) bool { return covers(pattern, importPath) })
}

func (l *allowlist) permitsModule(module string) bool {
	return slices.ContainsFunc(l.Exceptions, func(e *exception) bool { return slices.Contains(e.Modules, module) })
}

// permitsImport says whether an exception permits the import for that use, and
// records that the exception is still needed.
func (l *allowlist) permitsImport(importPath, use string) bool {
	permitted := false
	for _, exception := range l.Exceptions {
		if !coveredBy(exception.Imports, importPath) {
			continue
		}
		exception.used = true
		permitted = permitted || slices.Contains(exception.Uses, use)
	}
	return permitted
}

// useOf says what a Go file is: a test, a tool, or production code.
func useOf(file string) string {
	switch {
	case strings.HasSuffix(file, "_test.go") || strings.HasPrefix(file, "test/"):
		return tests
	case strings.HasPrefix(file, "hack/"):
		return tools
	}
	return production
}

func (l *allowlist) checkImports(repo fs.FS, module string) ([]string, error) {
	var problems []string
	files := token.NewFileSet()
	err := fs.WalkDir(repo, ".", func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if file != "." && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
				return fs.SkipDir
			}
			return nil
		}
		if path.Ext(name) != ".go" {
			return nil
		}
		text, err := fs.ReadFile(repo, file)
		if err != nil {
			return err
		}
		parsed, err := parser.ParseFile(files, file, text, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range parsed.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if reason := l.forbidden(importPath, module, useOf(file)); reason != "" {
				problems = append(problems, fmt.Sprintf("%s:%d: import %q %s",
					file, files.Position(spec.Pos()).Line, importPath, reason))
			}
		}
		return nil
	})
	return problems, err
}

// forbidden returns why a file with that use may not import the package, or
// the empty string when it may.
func (l *allowlist) forbidden(importPath, module, use string) string {
	host, _, _ := strings.Cut(importPath, "/")
	isStandardLibrary := !strings.Contains(host, ".")
	isOwn := importPath == module || strings.HasPrefix(importPath, module+"/")
	wraps := coveredBy(l.Wrappers, importPath)
	if isStandardLibrary || isOwn || (hostAllowed(importPath) && !wraps) {
		return ""
	}
	if l.permitsImport(importPath, use) {
		return ""
	}
	if wraps {
		return fmt.Sprintf("wraps a module that is not allowed, and no exception in %s permits it in %s", exceptionsPath, use)
	}
	return fmt.Sprintf("is not hosted at %s, and no exception in %s permits it in %s", allowedHost, exceptionsPath, use)
}

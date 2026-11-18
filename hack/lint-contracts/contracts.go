// lint-contracts checks the links between the sections of README.md and the
// tests enforcing them (AGENTS.md, "Tests and required checks"). A test links
// to a section with a comment naming its heading anchor:
//
//	// README: #heartbeat-contract
//
// The sections that no test links to are listed, with a reason, in
// unlinked-sections.txt, next to this file.
package main

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"unicode"
)

const (
	readmePath   = "README.md"
	unlinkedPath = "hack/lint-contracts/unlinked-sections.txt"

	// pending: the behaviour of the section is not implemented yet.
	pending = "pending"
	// informative: the section describes no testable behaviour.
	informative = "informative"
)

// section is a heading of the README.
type section struct {
	Anchor string
	Title  string
	Line   int
}

// link is one anchor of a "// README:" comment in a test file.
type link struct {
	Anchor   string
	Position string // file:line
}

// unlinked is an entry of the list of sections that no test links to.
type unlinked struct {
	Status string // pending or informative
	Anchor string
	Reason string
	Line   int
}

// lint returns the problems found in the repository: an empty result means
// that the README and the tests agree.
func lint(repo fs.FS) ([]string, error) {
	readme, err := fs.ReadFile(repo, readmePath)
	if err != nil {
		return nil, err
	}
	listText, err := fs.ReadFile(repo, unlinkedPath)
	if err != nil {
		return nil, err
	}
	links, problems, err := testLinks(repo)
	if err != nil {
		return nil, err
	}
	list, listProblems := parseUnlinked(string(listText))
	problems = append(problems, listProblems...)
	return append(problems, check(readmeSections(string(readme)), links, list)...), nil
}

// readmeSections returns the headings of a Markdown document, with the anchors
// GitHub gives them. Only "#" headings outside code blocks are recognised.
func readmeSections(markdown string) []section {
	var sections []section
	seen := map[string]int{}
	inCodeBlock := false
	for i, line := range strings.Split(markdown, "\n") {
		if isCodeFence(line) {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			continue
		}
		title, ok := headingTitle(line)
		if !ok {
			continue
		}
		anchor := githubAnchor(title)
		if n := seen[anchor]; n > 0 {
			seen[anchor]++
			anchor = fmt.Sprintf("%s-%d", anchor, n)
		} else {
			seen[anchor] = 1
		}
		sections = append(sections, section{Anchor: anchor, Title: title, Line: i + 1})
	}
	return sections
}

func isCodeFence(line string) bool {
	line = strings.TrimLeft(line, " ")
	return strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")
}

// headingTitle returns the text of a "#" heading: one to six "#", a space, the
// title, and optionally a closing run of "#".
func headingTitle(line string) (string, bool) {
	text := strings.TrimLeft(line, "#")
	level := len(line) - len(text)
	if level < 1 || level > 6 || !strings.HasPrefix(text, " ") {
		return "", false
	}
	text = strings.TrimSpace(text)
	if closing := strings.TrimRight(text, "#"); strings.HasSuffix(closing, " ") {
		text = strings.TrimSpace(closing)
	}
	return text, text != ""
}

// githubAnchor lowercases a heading, drops everything but letters, digits,
// hyphens and underscores, and replaces spaces by hyphens. Inline links and
// "_emphasis_" are not interpreted: keep them out of headings.
func githubAnchor(title string) string {
	var anchor strings.Builder
	for _, r := range strings.ToLower(title) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_':
			anchor.WriteRune(r)
		case r == ' ':
			anchor.WriteByte('-')
		}
	}
	return anchor.String()
}

// testLinks returns the README links of every test file of the repository, and
// the problems of the link comments that cannot be read. Like the go tool, it
// ignores testdata and the directories starting with "." or "_".
func testLinks(repo fs.FS) ([]link, []string, error) {
	var links []link
	var problems []string
	err := fs.WalkDir(repo, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != "." && isIgnoredDir(entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fileLinks, fileProblems, err := fileLinks(repo, path)
		links = append(links, fileLinks...)
		problems = append(problems, fileProblems...)
		return err
	})
	return links, problems, err
}

func isIgnoredDir(name string) bool {
	return name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// fileLinks reads the comments of a Go file with the Go parser, so that a
// link inside a string literal is not taken for a link.
func fileLinks(repo fs.FS, path string) ([]link, []string, error) {
	source, err := fs.ReadFile(repo, path)
	if err != nil {
		return nil, nil, err
	}
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, path, source, parser.ParseComments)
	if err != nil {
		return nil, nil, err
	}
	var links []link
	var problems []string
	for _, group := range file.Comments {
		for _, comment := range group.List {
			targets, ok := strings.CutPrefix(strings.TrimSpace(strings.TrimPrefix(comment.Text, "//")), "README:")
			if !ok {
				continue
			}
			position := fmt.Sprintf("%s:%d", path, files.Position(comment.Pos()).Line)
			anchors, ok := linkAnchors(targets)
			if !ok {
				problems = append(problems, fmt.Sprintf(
					"%s: malformed link %q: write it as \"// README: #heading-anchor\"", position, comment.Text))
				continue
			}
			for _, anchor := range anchors {
				links = append(links, link{Anchor: anchor, Position: position})
			}
		}
	}
	return links, problems, nil
}

// linkAnchors splits the anchors of a link comment: "#a, #b" or "#a #b".
func linkAnchors(targets string) ([]string, bool) {
	fields := strings.FieldsFunc(targets, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if len(fields) == 0 {
		return nil, false
	}
	var anchors []string
	for _, field := range fields {
		anchor, ok := strings.CutPrefix(field, "#")
		if !ok || anchor == "" {
			return nil, false
		}
		anchors = append(anchors, anchor)
	}
	return anchors, true
}

// parseUnlinked reads the list of unlinked sections. Each line is
// "<status> #<anchor> <reason>"; empty lines and lines starting with "#" are
// skipped.
func parseUnlinked(text string) ([]unlinked, []string) {
	var list []unlinked
	var problems []string
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		status, rest, _ := strings.Cut(line, " ")
		target, reason, _ := strings.Cut(strings.TrimSpace(rest), " ")
		anchor, hasHash := strings.CutPrefix(target, "#")
		reason = strings.TrimSpace(reason)
		if (status != pending && status != informative) || !hasHash || anchor == "" || reason == "" {
			problems = append(problems, fmt.Sprintf(
				"%s:%d: malformed line %q: write it as \"<%s|%s> #<anchor> <reason>\"",
				unlinkedPath, i+1, line, pending, informative))
			continue
		}
		list = append(list, unlinked{Status: status, Anchor: anchor, Reason: reason, Line: i + 1})
	}
	return list, problems
}

// firstLinks returns the first link to each section, and a problem for each
// link to something that is not a section.
func firstLinks(links []link, isSection map[string]bool) (map[string]string, []string) {
	var problems []string
	firstLink := map[string]string{}
	for _, l := range links {
		if !isSection[l.Anchor] {
			problems = append(problems, fmt.Sprintf(
				"%s: #%s is not a section of %s: was it renamed or removed?", l.Position, l.Anchor, readmePath))
			continue
		}
		if _, ok := firstLink[l.Anchor]; !ok {
			firstLink[l.Anchor] = l.Position
		}
	}
	return firstLink, problems
}

// check applies the rules: a link names an existing section, every section is
// linked or listed, and a pending section is not linked yet.
func check(sections []section, links []link, list []unlinked) []string {
	isSection := map[string]bool{}
	for _, s := range sections {
		isSection[s.Anchor] = true
	}

	firstLink, problems := firstLinks(links, isSection)

	listedAt := map[string]int{}
	for _, u := range list {
		position := fmt.Sprintf("%s:%d", unlinkedPath, u.Line)
		switch {
		case !isSection[u.Anchor]:
			problems = append(problems, fmt.Sprintf(
				"%s: #%s is not a section of %s: was it renamed or removed?", position, u.Anchor, readmePath))
		case listedAt[u.Anchor] != 0:
			problems = append(problems, fmt.Sprintf(
				"%s: #%s is already listed at line %d", position, u.Anchor, listedAt[u.Anchor]))
		case u.Status == pending && firstLink[u.Anchor] != "":
			problems = append(problems, fmt.Sprintf(
				"%s: #%s is listed as %s, but %s links to it: remove it from the list",
				position, u.Anchor, pending, firstLink[u.Anchor]))
		}
		if listedAt[u.Anchor] == 0 {
			listedAt[u.Anchor] = u.Line
		}
	}

	for _, s := range sections {
		if firstLink[s.Anchor] == "" && listedAt[s.Anchor] == 0 {
			problems = append(problems, fmt.Sprintf(
				"%s:%d: no test links to #%s (%q): link a test to it, or list it in %s as %s or %s",
				readmePath, s.Line, s.Anchor, s.Title, unlinkedPath, pending, informative))
		}
	}
	return problems
}

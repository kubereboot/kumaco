package main

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestReadmeSectionsUseGitHubAnchors(t *testing.T) {
	markdown := strings.Join([]string{
		"# kumaco - Kubernetes Maintenance Controller",
		"## Introduction",
		"### The kumaco.kured.dev MaintenanceWindow CRD",
		"### What does it reconcile, and when?",
		"### Multi-tenancy while doing self-service is *not an afterthought*",
		"### How kured relies on kumaco, and how to configure your maintenance window for kured:",
		"#### nodeSelector and trigger conflicts",
		"## Closed heading ##",
	}, "\n")

	want := []section{
		{Anchor: "kumaco---kubernetes-maintenance-controller", Title: "kumaco - Kubernetes Maintenance Controller", Line: 1},
		{Anchor: "introduction", Title: "Introduction", Line: 2},
		{Anchor: "the-kumacokureddev-maintenancewindow-crd", Title: "The kumaco.kured.dev MaintenanceWindow CRD", Line: 3},
		{Anchor: "what-does-it-reconcile-and-when", Title: "What does it reconcile, and when?", Line: 4},
		{Anchor: "multi-tenancy-while-doing-self-service-is-not-an-afterthought", Title: "Multi-tenancy while doing self-service is *not an afterthought*", Line: 5},
		{Anchor: "how-kured-relies-on-kumaco-and-how-to-configure-your-maintenance-window-for-kured", Title: "How kured relies on kumaco, and how to configure your maintenance window for kured:", Line: 6},
		{Anchor: "nodeselector-and-trigger-conflicts", Title: "nodeSelector and trigger conflicts", Line: 7},
		{Anchor: "closed-heading", Title: "Closed heading", Line: 8},
	}
	if got := readmeSections(markdown); !slices.Equal(got, want) {
		t.Errorf("readmeSections() =\n%v\nwant\n%v", got, want)
	}
}

func TestReadmeSectionsNumberDuplicateAnchors(t *testing.T) {
	got := anchors(readmeSections("## Usage\n### Usage\n#### Usage\n"))
	want := []string{"usage", "usage-1", "usage-2"}
	if !slices.Equal(got, want) {
		t.Errorf("anchors = %v, want %v", got, want)
	}
}

func TestReadmeSectionsIgnoreNonHeadings(t *testing.T) {
	markdown := strings.Join([]string{
		"## Real",
		"```yaml",
		"# a comment in a code block",
		"```",
		"~~~",
		"## also in a code block",
		"~~~",
		"#hashtag, not a heading",
		"text with a # in it",
		"####### seven hashes are not a heading",
	}, "\n")

	got := anchors(readmeSections(markdown))
	if want := []string{"real"}; !slices.Equal(got, want) {
		t.Errorf("anchors = %v, want %v", got, want)
	}
}

func TestTestLinksReadsCommentsOfTestFiles(t *testing.T) {
	repo := fstest.MapFS{
		"internal/window/window_test.go": {Data: []byte(`package window

// README: #heartbeat-contract
func TestOne(t *testing.T) {}

// TestTwo enforces two sections.
// README: #time-zones, #cel-triggers
// README: #gotchas #metrics
func TestTwo(t *testing.T) {
	_ = "// README: #in-a-string"
}
`)},
		// Only tests enforce contracts: other files are not read.
		"internal/window/window.go": {Data: []byte("package window\n\n// README: #production-code\n")},
		// Directories the go tool ignores are not read either.
		"internal/window/testdata/fixture_test.go": {Data: []byte("package fixture\n\n// README: #fixture\n")},
		"_output/copy_test.go":                     {Data: []byte("package output\n\n// README: #output\n")},
		".cache/copy_test.go":                      {Data: []byte("package cache\n\n// README: #cache\n")},
	}

	links, problems, err := testLinks(repo)
	if err != nil {
		t.Fatalf("testLinks() error: %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("testLinks() problems = %v, want none", problems)
	}
	want := []link{
		{Anchor: "heartbeat-contract", Position: "internal/window/window_test.go:3"},
		{Anchor: "time-zones", Position: "internal/window/window_test.go:7"},
		{Anchor: "cel-triggers", Position: "internal/window/window_test.go:7"},
		{Anchor: "gotchas", Position: "internal/window/window_test.go:8"},
		{Anchor: "metrics", Position: "internal/window/window_test.go:8"},
	}
	if !slices.Equal(links, want) {
		t.Errorf("testLinks() =\n%v\nwant\n%v", links, want)
	}
}

func TestTestLinksReportsMalformedLinks(t *testing.T) {
	for name, comment := range map[string]string{
		"no anchor":       "// README:",
		"missing hash":    "// README: heartbeat-contract",
		"hash alone":      "// README: #",
		"one of two bad":  "// README: #gotchas metrics",
		"heading, no tag": "// README: Heartbeat contract",
	} {
		t.Run(name, func(t *testing.T) {
			repo := fstest.MapFS{"a_test.go": {Data: []byte("package a\n\n" + comment + "\n")}}
			_, problems, err := testLinks(repo)
			if err != nil {
				t.Fatalf("testLinks() error: %v", err)
			}
			if len(problems) != 1 || !strings.HasPrefix(problems[0], "a_test.go:3: ") {
				t.Errorf("testLinks() problems = %q, want one problem at a_test.go:3", problems)
			}
		})
	}
}

func TestTestLinksFailsOnUnparsableTestFile(t *testing.T) {
	repo := fstest.MapFS{"a_test.go": {Data: []byte("this is not Go\n")}}
	if _, _, err := testLinks(repo); err == nil {
		t.Error("testLinks() error = nil, want a parse error")
	}
}

func TestParseUnlinkedSections(t *testing.T) {
	text := strings.Join([]string{
		"# A comment, then an empty line.",
		"",
		"informative #introduction  An overview: nothing to test.",
		"pending     #metrics       The controller does not exist yet.",
	}, "\n")

	got, problems := parseUnlinked(text)
	if len(problems) != 0 {
		t.Errorf("parseUnlinked() problems = %v, want none", problems)
	}
	want := []unlinked{
		{Status: "informative", Anchor: "introduction", Reason: "An overview: nothing to test.", Line: 3},
		{Status: "pending", Anchor: "metrics", Reason: "The controller does not exist yet.", Line: 4},
	}
	if !slices.Equal(got, want) {
		t.Errorf("parseUnlinked() =\n%v\nwant\n%v", got, want)
	}
}

func TestParseUnlinkedSectionsReportsBadLines(t *testing.T) {
	for name, line := range map[string]string{
		"unknown status": "skipped #metrics Because.",
		"missing hash":   "pending metrics Because.",
		"missing reason": "pending #metrics",
		"status alone":   "pending",
	} {
		t.Run(name, func(t *testing.T) {
			entries, problems := parseUnlinked(line + "\n")
			if len(entries) != 0 {
				t.Errorf("parseUnlinked() entries = %v, want none", entries)
			}
			if len(problems) != 1 || !strings.HasPrefix(problems[0], unlinkedPath+":1: ") {
				t.Errorf("parseUnlinked() problems = %q, want one problem at line 1", problems)
			}
		})
	}
}

// The rules of AGENTS.md, "Tests and required checks".
func TestCheck(t *testing.T) {
	sections := []section{
		{Anchor: "heartbeat-contract", Title: "Heartbeat contract", Line: 10},
		{Anchor: "metrics", Title: "Metrics", Line: 20},
		{Anchor: "getting-help", Title: "Getting Help", Line: 30},
	}
	heartbeatLink := link{Anchor: "heartbeat-contract", Position: "a_test.go:3"}
	metricsLink := link{Anchor: "metrics", Position: "b_test.go:5"}
	pendingMetrics := unlinked{Status: "pending", Anchor: "metrics", Reason: "Not implemented.", Line: 1}
	informativeHelp := unlinked{Status: "informative", Anchor: "getting-help", Reason: "Contact details.", Line: 2}

	tests := []struct {
		name     string
		links    []link
		unlinked []unlinked
		want     []string // a substring of each expected problem, in order
	}{
		{
			name:     "every section is linked or listed",
			links:    []link{heartbeatLink},
			unlinked: []unlinked{pendingMetrics, informativeHelp},
		},
		{
			name:     "every section is linked",
			links:    []link{heartbeatLink, metricsLink, {Anchor: "getting-help", Position: "c_test.go:1"}},
			unlinked: nil,
		},
		{
			name:     "a link to a section that does not exist",
			links:    []link{heartbeatLink, {Anchor: "heartbeat-contracts", Position: "a_test.go:9"}},
			unlinked: []unlinked{pendingMetrics, informativeHelp},
			want:     []string{"a_test.go:9: #heartbeat-contracts is not a section of README.md"},
		},
		{
			name:     "a section that is neither linked nor listed",
			links:    []link{heartbeatLink},
			unlinked: []unlinked{informativeHelp},
			want:     []string{`README.md:20: no test links to #metrics ("Metrics")`},
		},
		{
			name:     "a pending section that a test links to",
			links:    []link{heartbeatLink, metricsLink},
			unlinked: []unlinked{pendingMetrics, informativeHelp},
			want:     []string{unlinkedPath + ":1: #metrics is listed as pending, but b_test.go:5 links to it"},
		},
		{
			name:     "a listed section that does not exist",
			links:    []link{heartbeatLink},
			unlinked: []unlinked{pendingMetrics, informativeHelp, {Status: "pending", Anchor: "renamed", Reason: "Gone.", Line: 3}},
			want:     []string{unlinkedPath + ":3: #renamed is not a section of README.md"},
		},
		{
			name:     "a section listed twice",
			links:    []link{heartbeatLink},
			unlinked: []unlinked{pendingMetrics, informativeHelp, {Status: "informative", Anchor: "metrics", Reason: "Again.", Line: 3}},
			want:     []string{unlinkedPath + ":3: #metrics is already listed at line 1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := check(sections, tt.links, tt.unlinked)
			if len(got) != len(tt.want) {
				t.Fatalf("check() = %q, want %d problems", got, len(tt.want))
			}
			for i, want := range tt.want {
				if !strings.Contains(got[i], want) {
					t.Errorf("check()[%d] = %q, want it to contain %q", i, got[i], want)
				}
			}
		})
	}
}

func TestLintReadsTheRepository(t *testing.T) {
	repo := fstest.MapFS{
		"README.md":  {Data: []byte("# Project\n## Heartbeat contract\n## Metrics\n")},
		unlinkedPath: {Data: []byte("informative #project The title.\n")},
		"a_test.go":  {Data: []byte("package a\n\n// README: #heartbeat-contract\n")},
	}

	problems, err := lint(repo)
	if err != nil {
		t.Fatalf("lint() error: %v", err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "#metrics") {
		t.Errorf("lint() = %q, want one problem about #metrics", problems)
	}
}

func TestLintFailsWithoutItsInputs(t *testing.T) {
	for name, repo := range map[string]fstest.MapFS{
		"no README": {unlinkedPath: {Data: []byte("")}},
		"no list":   {"README.md": {Data: []byte("# Project\n")}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := lint(repo); err == nil {
				t.Error("lint() error = nil, want an error")
			}
		})
	}
}

func anchors(sections []section) []string {
	var out []string
	for _, s := range sections {
		out = append(out, s.Anchor)
	}
	return out
}

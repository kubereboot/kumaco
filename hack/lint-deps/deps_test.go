package main

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

const goMod = `module github.com/kubereboot/kumaco

go 1.27.4

require (
	k8s.io/api v0.37.2
	sigs.k8s.io/controller-runtime v0.25.3
)

require (
	github.com/go-logr/logr v1.4.3 // indirect
	go.uber.org/zap v1.27.0 // indirect
)
`

const noExceptions = `{"wrappers": ["sigs.k8s.io/controller-runtime/pkg/log/zap", "k8s.io/apiserver/pkg/cel/..."], "exceptions": []}`

var today = time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)

func source(imports ...string) string {
	text := "package x\n"
	for _, path := range imports {
		text += "import _ \"" + path + "\"\n"
	}
	return text
}

func repository(files map[string]string) fstest.MapFS {
	repo := fstest.MapFS{}
	for name, text := range files {
		repo[name] = &fstest.MapFile{Data: []byte(text)}
	}
	if _, ok := repo[goModPath]; !ok {
		repo[goModPath] = &fstest.MapFile{Data: []byte(goMod)}
	}
	if _, ok := repo[exceptionsPath]; !ok {
		repo[exceptionsPath] = &fstest.MapFile{Data: []byte(noExceptions)}
	}
	return repo
}

func zapException(reviewed, nextReview string, uses string) string {
	return `{"wrappers": ["sigs.k8s.io/controller-runtime/pkg/log/zap"], "exceptions": [{
		"name": "zap",
		"imports": ["sigs.k8s.io/controller-runtime/pkg/log/zap"],
		"uses": [` + uses + `],
		"reason": "the logging backend that Kubebuilder scaffolds",
		"approval": "maintainer, in the review of the change",
		"reviewed": "` + reviewed + `", "nextReview": "` + nextReview + `"}]}`
}

// AGENTS.md, "Dependencies and versions".
func TestLint(t *testing.T) {
	for _, tt := range []struct {
		name  string
		files map[string]string
		// want is a part of each expected problem; none when empty.
		want []string
	}{
		{
			name: "the standard library, this repository, and the Kubernetes hosts are allowed",
			files: map[string]string{"internal/x/x.go": source(
				"fmt", "net/http", "go/parser",
				"github.com/kubereboot/kumaco/api/v1alpha1",
				"k8s.io/api/core/v1", "sigs.k8s.io/controller-runtime/pkg/client", "sigs.k8s.io/yaml",
			)},
		},
		{
			name:  "a lookalike of the allowed host",
			files: map[string]string{"internal/x/x.go": source("k8s.io.example.org/api")},
			want:  []string{`internal/x/x.go:2: import "k8s.io.example.org/api"`},
		},
		{
			name:  "a host that only ends like the allowed host",
			files: map[string]string{"internal/x/x.go": source("notk8s.io/api", "example.org/k8s.io/api")},
			want:  []string{`import "notk8s.io/api"`, `import "example.org/k8s.io/api"`},
		},
		{
			name:  "a forbidden dependency",
			files: map[string]string{"internal/x/x.go": source("github.com/stretchr/testify/assert")},
			want:  []string{`import "github.com/stretchr/testify/assert"`},
		},
		{
			name:  "a forbidden direct import of a transitive dependency",
			files: map[string]string{"internal/x/x.go": source("github.com/go-logr/logr")},
			want:  []string{`import "github.com/go-logr/logr"`},
		},
		{
			name:  "a forbidden import in a test",
			files: map[string]string{"internal/x/x_test.go": source("github.com/onsi/gomega")},
			want:  []string{`internal/x/x_test.go:2: import "github.com/onsi/gomega"`},
		},
		{
			name: "a forbidden import behind a build tag",
			files: map[string]string{"test/e2e/x_test.go": "//go:build e2e\n\n" +
				source("github.com/onsi/ginkgo/v2")},
			want: []string{`import "github.com/onsi/ginkgo/v2"`},
		},
		{
			name:  "a forbidden import in a tool",
			files: map[string]string{"hack/tool/main.go": source("golang.org/x/mod/modfile")},
			want:  []string{`import "golang.org/x/mod/modfile"`},
		},
		{
			name: "a package of an allowed host that wraps a forbidden module",
			files: map[string]string{"cmd/main.go": source(
				"sigs.k8s.io/controller-runtime/pkg/log/zap", "k8s.io/apiserver/pkg/cel/environment", "k8s.io/apiserver/pkg/cel",
			)},
			want: []string{
				`import "sigs.k8s.io/controller-runtime/pkg/log/zap"`,
				`import "k8s.io/apiserver/pkg/cel/environment"`,
				`import "k8s.io/apiserver/pkg/cel"`,
			},
		},
		{
			name: "a forbidden direct module",
			files: map[string]string{goModPath: goMod + "\nrequire github.com/google/cel-go v0.29.2\n" +
				"require k8s.io.example.org/api v1.0.0\n"},
			want: []string{`module "github.com/google/cel-go"`, `module "k8s.io.example.org/api"`},
		},
		{
			name: "an approved exception",
			files: map[string]string{
				exceptionsPath:      zapException("2026-11-17", "2027-11-17", `"production", "tests"`),
				"cmd/main.go":       source("sigs.k8s.io/controller-runtime/pkg/log/zap"),
				"cmd/main_test.go":  source("sigs.k8s.io/controller-runtime/pkg/log/zap"),
				"internal/x/doc.go": source("fmt"),
			},
		},
		{
			name: "an exception does not cover another use",
			files: map[string]string{
				exceptionsPath:     zapException("2026-11-17", "2027-11-17", `"tests"`),
				"cmd/main.go":      source("sigs.k8s.io/controller-runtime/pkg/log/zap"),
				"cmd/main_test.go": source("sigs.k8s.io/controller-runtime/pkg/log/zap"),
				"hack/x/main.go":   source("sigs.k8s.io/controller-runtime/pkg/log/zap"),
			},
			want: []string{`cmd/main.go:2: import "sigs.k8s.io/controller-runtime/pkg/log/zap"`, `hack/x/main.go:2: import`},
		},
		{
			name: "an exception does not cover the module it wraps",
			files: map[string]string{
				exceptionsPath: zapException("2026-11-17", "2027-11-17", `"production"`),
				"cmd/main.go":  source("sigs.k8s.io/controller-runtime/pkg/log/zap", "go.uber.org/zap", "go.uber.org/zap/zapcore"),
			},
			want: []string{`import "go.uber.org/zap"`, `import "go.uber.org/zap/zapcore"`},
		},
		{
			name: "an overdue exception review",
			files: map[string]string{
				exceptionsPath: zapException("2025-11-17", "2026-11-17", `"production"`),
				"cmd/main.go":  source("sigs.k8s.io/controller-runtime/pkg/log/zap"),
			},
			want: []string{`exception "zap": its review was due on 2026-11-17`},
		},
		{
			name: "a review due more than one year after the previous one",
			files: map[string]string{
				exceptionsPath: zapException("2026-11-17", "2027-11-18", `"production"`),
				"cmd/main.go":  source("sigs.k8s.io/controller-runtime/pkg/log/zap"),
			},
			want: []string{`exception "zap": its next review (2027-11-18) is more than one year after the previous one (2026-11-17)`},
		},
		{
			name: "an exception that nothing uses anymore",
			files: map[string]string{
				exceptionsPath: zapException("2026-11-17", "2027-11-17", `"production"`),
				"cmd/main.go":  source("fmt"),
			},
			want: []string{`exception "zap": nothing imports it anymore`},
		},
		{
			name: "an incomplete exception",
			files: map[string]string{
				exceptionsPath: `{"exceptions": [{"name": "zap", "imports": ["go.uber.org/zap"], "uses": ["everywhere", "production"],
					"reviewed": "2026-11-17", "nextReview": "soon"}]}`,
				"cmd/main.go": source("go.uber.org/zap"),
			},
			want: []string{`"zap": unknown use "everywhere"`, `"zap": no reason`, `"zap": no approval`, `"zap": nextReview`},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			problems, err := lint(repository(tt.files), today)
			if err != nil {
				t.Fatal(err)
			}
			all := strings.Join(problems, "\n")
			for _, want := range tt.want {
				if !strings.Contains(all, want) {
					t.Errorf("no problem mentions %q", want)
				}
			}
			if len(problems) != len(tt.want) {
				t.Errorf("%d problem(s), want %d:\n%s", len(problems), len(tt.want), all)
			}
		})
	}
}

func TestAnExceptionCoversItsModuleAndPackageTree(t *testing.T) {
	repo := repository(map[string]string{
		goModPath: goMod + "\nrequire github.com/google/cel-go v0.29.2\n",
		exceptionsPath: `{"wrappers": ["k8s.io/apiserver/pkg/cel/..."], "exceptions": [{
			"name": "cel",
			"modules": ["github.com/google/cel-go"],
			"imports": ["github.com/google/cel-go/cel", "k8s.io/apiserver/pkg/cel/..."],
			"uses": ["production", "tests"], "reason": "CEL triggers", "approval": "maintainer",
			"reviewed": "2026-11-17", "nextReview": "2027-11-17"}]}`,
		"internal/trigger/trigger.go": source(
			"github.com/google/cel-go/cel", "k8s.io/apiserver/pkg/cel", "k8s.io/apiserver/pkg/cel/environment",
			"github.com/google/cel-go/interpreter",
		),
	})
	problems, err := lint(repo, today)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], `import "github.com/google/cel-go/interpreter"`) {
		t.Errorf("problems = %q, want only the cel-go package outside of the exception", problems)
	}
}

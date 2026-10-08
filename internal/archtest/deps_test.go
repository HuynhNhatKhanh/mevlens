// Package archtest enforces the architecture's dependency rule on the full
// (transitive) import graph. golangci-lint's depguard only sees direct imports;
// this test also catches a package that reaches infrastructure through an
// intermediate package, e.g. classify → registry → rpc.
package archtest

import (
	"os/exec"
	"strings"
	"testing"
)

const module = "github.com/huynhnhatkhanh/mevlens/internal/"

type rule struct {
	internal []string // forbidden packages under internal/ (and their subpackages)
	external []string // forbidden import-path prefixes outside the module
}

var (
	infra = []string{"net/http", "github.com/ClickHouse/"}
	core  = rule{internal: []string{"rpc", "registry", "observe", "store", "telemetry", "config", "fixture"}, external: infra}
)

var rules = map[string]rule{
	// Deterministic core: pure domain logic, no infrastructure.
	"eth":      core,
	"dex":      core,
	"pricing":  core,
	"classify": core,
	// Use case: reaches infrastructure only through its ports (interfaces).
	"observe": {internal: []string{"rpc", "store", "telemetry", "config", "fixture"}, external: infra},
	// Adapters must not reach into each other, nor into the use case.
	"rpc":              {internal: []string{"registry", "observe", "classify", "store", "telemetry", "config"}},
	"registry":         {internal: []string{"observe", "classify", "store", "telemetry", "config"}},
	"store/clickhouse": {internal: []string{"rpc", "registry", "telemetry", "config"}},
}

func TestDependencyRule(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	for pkg, r := range rules {
		out, err := exec.CommandContext(t.Context(), goBin, "list", "-deps", "-f", "{{.ImportPath}}", module+pkg).Output()
		if err != nil {
			t.Fatalf("go list %s: %v", pkg, err)
		}
		for _, dep := range strings.Fields(string(out)) {
			for _, f := range r.internal {
				if p := module + f; dep == p || strings.HasPrefix(dep, p+"/") {
					t.Errorf("%s must not depend on internal/%s (transitively imports %s)", pkg, f, dep)
				}
			}
			for _, f := range r.external {
				if dep == strings.TrimSuffix(f, "/") || strings.HasPrefix(dep, f) {
					t.Errorf("%s must not depend on %s (transitively imports %s)", pkg, f, dep)
				}
			}
		}
	}
}

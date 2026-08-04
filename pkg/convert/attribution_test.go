/*
Copyright 2024 The KubeZoo Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package convert

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestNoBackwardRefusesAnUnattributableReference is a structural guard for a
// defect class this repository has now met four times.
//
// A Backward that returns an error because a reference lacks the tenant prefix
// does not hide a field: a convertor error fails the whole object, and one
// failed object fails the whole LIST. The tenant loses sight of everything of
// that kind, and cannot delete what it cannot read.
//
// ⭐ Written as a source walk rather than as behaviour, deliberately. Behaviour
// tests need someone to think of the case -- and each of the four instances was
// found by running the product, not by a unit test. The pattern is what recurs,
// so the pattern is what is checked: a HasPrefix on tenantID guarding a return
// of an error, anywhere in a Backward.
//
// ⚠️ If a new site legitimately must refuse, the answer is not to add it to a
// list here. Hide the object instead -- the read paths already filter what a
// tenant may see -- and leave conversion able to succeed.
func TestNoBackwardRefusesAnUnattributableReference(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// ⛔ Not only methods literally called Backward. This walked those alone,
			// and backwardWebhookClientConfig -- a plain function that Backward calls
			// three lines later -- refused an unattributable namespace the whole time.
			// The same blind spot as TestEveryWritePathRunsTheSameGuards reading one
			// file: the check's own COVERAGE was never checked, twice in one day.
			//
			// ⚠️ Matched by name rather than by following the call graph, on purpose.
			// A call-graph walk would also drag in Forward's helpers, where refusing is
			// correct -- a tenant writing a bad reference SHOULD be told so. It is only
			// the READ direction where refusing costs a tenant sight of everything.
			if fn.Name.Name != "Backward" && !strings.HasPrefix(fn.Name.Name, "backward") {
				continue
			}
			checked++
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				// ⚠️ Do not walk into a nested function. rewriteManagedFieldVersions
				// takes a closure that returns a STRING, and a first version of this
				// test read its `return version` as "returns an error" -- a false
				// positive, which is worse than no test at all: it gets allowlisted,
				// and then it never says anything true again.
				if _, isFunc := n.(*ast.FuncLit); isFunc {
					return false
				}
				ifStmt, ok := n.(*ast.IfStmt)
				if !ok || !guardsOnTenantPrefix(ifStmt.Cond) || !returnsError(ifStmt.Body, fn) {
					return true
				}
				t.Errorf("%s: %s refuses a reference that does not carry the tenant prefix.\n"+
					"A convertor error fails the whole object, and one failed object fails the "+
					"whole list -- the tenant can then neither see nor delete anything of this "+
					"kind. Use trimIfAttributable, which leaves what it cannot attribute alone.",
					fset.Position(ifStmt.Pos()), name)
				return true
			})
		}
	}
	// ⚠️ A walk that finds no Backward at all would pass in silence, which is the
	// failure this test exists to prevent.
	if checked < 10 {
		t.Fatalf("only %d backward-direction functions were walked; the parse is not finding them", checked)
	}
}

// guardsOnTenantPrefix reports whether a condition tests a tenant prefix, in
// either the positive or the negated form.
func guardsOnTenantPrefix(cond ast.Expr) bool {
	found := false
	ast.Inspect(cond, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "HasPrefix" {
			return true
		}
		for _, arg := range call.Args[1:] {
			if mentionsTenantID(arg) {
				found = true
			}
		}
		return true
	})
	return found
}

func mentionsTenantID(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "tenantID" {
			found = true
		}
		return true
	})
	return found
}

// returnsError reports whether a block returns a non-nil error from fn.
//
// ⚠️ Decided from fn's SIGNATURE, not from a fixed result count. A first version
// required exactly two results, because a Backward returns (runtime.Object,
// error) -- and that quietly stopped seeing helpers that return an error alone,
// which is what backwardWebhookClientConfig is. Requiring two results fixed a
// false positive and bought a false negative, which is the same trade
// TestEveryWritePathRunsTheSameGuards went through: direct calls only were too
// noisy, the full closure too quiet, and neither said anything true.
func returnsError(body *ast.BlockStmt, fn *ast.FuncDecl) bool {
	results := fn.Type.Results
	if results == nil || len(results.List) == 0 {
		return false
	}
	last := results.List[len(results.List)-1]
	id, ok := last.Type.(*ast.Ident)
	if !ok || id.Name != "error" {
		return false
	}
	want := 0
	for _, f := range results.List {
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		want += n
	}

	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if _, isFunc := n.(*ast.FuncLit); isFunc {
			return false
		}
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != want {
			return true
		}
		last := ret.Results[len(ret.Results)-1]
		if id, ok := last.(*ast.Ident); ok && id.Name == "nil" {
			return true
		}
		found = true
		return true
	})
	return found
}

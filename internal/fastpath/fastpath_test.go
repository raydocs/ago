package fastpath

import "testing"

func TestDetect(t *testing.T) {
	for _, prompt := range []string{
		"Fix internal/catalog/catalog.go and run go test ./internal/catalog",
		"update `src/parser.ts`, then verify with npm test",
	} {
		if !Detect(prompt) {
			t.Fatalf("expected fast path: %q", prompt)
		}
	}
	for _, prompt := range []string{
		"fix the parser",
		"fix parser.go and test it",
		"fix a.go and b.go then test",
		"update schema.go and deploy the database migration; test it",
		"research parser.go and verify sources",
		"fix parser.go and run go test ./parser || true",
	} {
		if Detect(prompt) {
			t.Fatalf("unexpected fast path: %q", prompt)
		}
	}
}

func TestParseFreezesTargetAndVerifier(t *testing.T) {
	contract, ok := Parse("Fix internal/catalog/catalog.go and run go test ./internal/catalog")
	if !ok || contract.TargetPath != "internal/catalog/catalog.go" || contract.Verifier != "go test ./internal/catalog" {
		t.Fatalf("unexpected contract: %#v ok=%v", contract, ok)
	}
	contract, ok = Parse("Fix calc.go and run go test ./...")
	if !ok || contract.Verifier != "go test ./..." {
		t.Fatalf("Go recursive package pattern was not preserved: %#v ok=%v", contract, ok)
	}
}

func TestSafeVerifierRejectsShellComposition(t *testing.T) {
	for _, command := range []string{
		"go test ./... || true",
		"go test ./... && git status",
		"pytest; echo ok",
		"npm test > /tmp/test.log",
		"npx vitest &",
	} {
		if SafeVerifier(command) {
			t.Fatalf("unsafe verifier accepted: %q", command)
		}
	}
}

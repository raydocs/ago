package mcpserver

import (
	"path/filepath"
	"strings"
)

// Domain tags used only for composite-slice admission. A Worker must own one
// independently verifiable slice; multi-domain packets are rejected before any
// model call instead of raising MaxTurns.
const (
	domainSchema  = "schema_migration"
	domainAPI     = "api_service"
	domainUI      = "ui_frontend"
	domainUsage   = "usage_metrics"
	domainDeploy  = "deploy_ops"
	domainParsing = "transcript_pipeline"
	domainUnknown = "unknown"
)

// detectSliceDomains classifies the worker packet into coarse work domains.
//
// Priority (v1.4.5):
//  1. Write paths — authoritative owned surface
//  2. done_condition / objective only when paths are empty (read-only scout rare)
//  3. Context / marginal / “not in scope” prose never adds domains (avoids
//     “API is out of scope” false positives and Usage-page UI false denies)
//  4. Unknown-only paths stay domainUnknown (never fabricated API+UI)
//  5. known + unknown is multi-domain and can reject
func detectSliceDomains(in WorkerStartInput) []string {
	seen := map[string]bool{}
	add := func(domain string) {
		if domain != "" {
			seen[domain] = true
		}
	}

	nonEmptyPaths := 0
	classifiedPaths := 0
	unknownPaths := 0
	var unknownList []string
	for _, path := range in.Paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		nonEmptyPaths++
		if d := domainFromPath(path); d != "" {
			add(d)
			classifiedPaths++
		} else {
			unknownPaths++
			unknownList = append(unknownList, path)
		}
	}

	// Paths-only classification is allowed only when every non-empty path is
	// classified. Partial classification (UI path + unknown backend path) must
	// merge objective/done_condition or be treated conservatively.
	if nonEmptyPaths > 0 && unknownPaths == 0 {
		return orderedDomains(seen)
	}

	// Path-less packets: use done_condition + objective only (not context/marginal).
	if nonEmptyPaths == 0 {
		text := stripNegatedScope(strings.ToLower(strings.Join([]string{
			in.Objective,
			in.DoneCondition,
		}, "\n")))
		if containsAnyFold(text, "migration", "migrations/", ".sql", "alter table", "create table") {
			add(domainSchema)
		}
		if containsAnyFold(text, "api route", "api handler", "thread-app/src", "hono", "express") {
			add(domainAPI)
		}
		if containsAnyFold(text, "styles.css", "app.js", "index.html", "playwright", "viewport", "ui polish", "mobile layout") {
			add(domainUI)
		}
		// usage foundation (data plane), not the word "Usage" in a page title alone.
		if containsAnyFold(text, "token bucket", "cache_read", "usage parser", "threadusage", "usage foundation", "usage_records") {
			add(domainUsage)
		}
		if containsAnyFold(text, "wrangler deploy", "cloudflare deploy", "production deploy") {
			add(domainDeploy)
		}
		if containsAnyFold(text, "threadgraph", "threadsync", "jsonl parser", "compact_boundary") {
			add(domainParsing)
		}
		return orderedDomains(seen)
	}

	// Unknown-only paths: single structural package → domainUnknown (admit).
	// Never invent api+ui composite for a lone internal/package file.
	if unknownPaths > 0 && classifiedPaths == 0 {
		add(domainUnknown)
		return orderedDomains(seen)
	}

	// Partial: known paths + unknown paths → keep known domains and mark unknown.
	// Text may still add domains for the unknown side when objective is explicit.
	if unknownPaths > 0 && classifiedPaths > 0 {
		add(domainUnknown)
		text := stripNegatedScope(strings.ToLower(strings.Join([]string{
			in.Objective,
			in.DoneCondition,
		}, "\n")))
		if containsAnyFold(text, "migration", "migrations/", ".sql", "alter table", "create table") {
			add(domainSchema)
		}
		if containsAnyFold(text, "api route", "api handler", "thread-app/src", "hono", "express", "backend") {
			add(domainAPI)
		}
		if containsAnyFold(text, "styles.css", "app.js", "index.html", "playwright", "viewport", "ui polish", "mobile layout") {
			add(domainUI)
		}
		if containsAnyFold(text, "token bucket", "cache_read", "usage parser", "threadusage", "usage foundation", "usage_records") {
			add(domainUsage)
		}
		if containsAnyFold(text, "wrangler deploy", "cloudflare deploy", "production deploy") {
			add(domainDeploy)
		}
		if containsAnyFold(text, "threadgraph", "threadsync", "jsonl parser", "compact_boundary") {
			add(domainParsing)
		}
		// Ensure mixed known+unknown is multi-domain even if text is vague.
		// (e.g. UI path + cmd/backend.go with "Update UI and backend")
		if len(seen) < 2 {
			// If only UI+unknown with no extra domain, still multi via unknown.
			// orderedDomains will include both when UI was from path and unknown added.
		}
		_ = unknownList
		return orderedDomains(seen)
	}

	return orderedDomains(seen)
}

func orderedDomains(seen map[string]bool) []string {
	order := []string{domainSchema, domainAPI, domainUI, domainUsage, domainDeploy, domainParsing, domainUnknown}
	out := make([]string, 0, len(seen))
	for _, d := range order {
		if seen[d] {
			out = append(out, d)
		}
	}
	return out
}

// stripNegatedScope removes clauses that declare out-of-scope work so they do
// not contribute keyword domains (best-effort, English + Chinese).
func stripNegatedScope(text string) string {
	// Split on sentence-ish separators and drop lines that are pure exclusions.
	parts := strings.FieldsFunc(text, func(r rune) bool {
		return r == '.' || r == ';' || r == '。' || r == '；' || r == '\n'
	})
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if isExclusionClause(p) {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(kept, ". ")
}

func isExclusionClause(p string) bool {
	markers := []string{
		"out of scope", "not in scope", "outside scope", "frozen and not",
		"do not modify", "do not touch", "must not change", "without changing",
		"excluding", "except for", "不在范围", "不在本次", "不要改", "无需修改", "已冻结",
	}
	for _, m := range markers {
		if strings.Contains(p, m) {
			return true
		}
	}
	return false
}

func domainFromPath(path string) string {
	p := strings.ToLower(filepath.ToSlash(strings.TrimSpace(path)))
	if p == "" {
		return ""
	}
	base := filepath.Base(p)
	switch {
	case strings.Contains(p, "/migrations/") || strings.HasSuffix(p, ".sql"):
		return domainSchema
	case strings.Contains(p, "thread-app/public/") || base == "styles.css" || base == "app.js" || base == "index.html" || base == "shell-model.mjs":
		return domainUI
	case strings.Contains(p, "thread-app/src/"):
		return domainAPI
	case strings.Contains(p, "internal/threadusage/"):
		return domainUsage
	case strings.Contains(p, "internal/threadgraph/") || strings.Contains(p, "internal/threadsync/") || strings.Contains(p, "internal/threadread/") || strings.Contains(p, "internal/threadfind/"):
		return domainParsing
	case strings.Contains(p, "wrangler"):
		return domainDeploy
	// Bare "usage" path segments no longer force domainUsage — too many UI paths
	// mention usage pages. Prefer internal/threadusage or explicit data-plane text.
	default:
		return ""
	}
}

// structuralPackageKey returns a coarse package key for unknown-path grouping.
// internal/foo/bar.go and internal/foo/baz.go share "internal/foo";
// cmd/a.go and internal/b.go do not share a key.
func structuralPackageKey(path string) string {
	p := filepath.ToSlash(strings.TrimSpace(path))
	p = strings.TrimPrefix(p, "./")
	parts := strings.Split(p, "/")
	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	// Drop drive-like prefixes if absolute under repo-relative use.
	if parts[0] == "" && len(parts) > 1 {
		parts = parts[1:]
	}
	top := parts[0]
	switch top {
	case "internal", "pkg", "packages", "src", "lib", "app", "apps", "services", "cmd":
		if len(parts) >= 2 {
			return top + "/" + parts[1]
		}
		return top
	default:
		return top
	}
}

// unknownOnlySamePackage reports whether all non-empty paths are unknown and
// share one structural package (admits as a single unknown domain).
func unknownOnlySamePackage(paths []string) bool {
	var key string
	n := 0
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if domainFromPath(path) != "" {
			return false
		}
		k := structuralPackageKey(path)
		if k == "" {
			return false
		}
		if n == 0 {
			key = k
		} else if k != key {
			return false
		}
		n++
	}
	return n > 0
}

// compositeSliceReason returns a rejection reason when the packet spans too
// many independent domains, or pairs that historically caused max-turn waste.
//
// UI-only paths never pair-deny with usage: a Usage *page* polish slice that only
// owns public assets is a single UI domain after path-first classification.
//
// Unknown-only within one structural package is admitted (single domain).
// Unknown spanning multiple top-level packages is rejected.
// Known domain + unknown is always a composite reject.
func compositeSliceReason(domains []string) string {
	if len(domains) >= 3 {
		return "composite_slice: objective spans " + strings.Join(domains, "+") + "; split into one independently verifiable domain per Worker (do not raise max turns)"
	}
	has := map[string]bool{}
	for _, d := range domains {
		has[d] = true
	}
	// known + unknown is composite regardless of which known domain.
	if has[domainUnknown] {
		known := 0
		for _, d := range domains {
			if d != domainUnknown {
				known++
			}
		}
		if known > 0 {
			return "composite_slice: uncategorized paths mixed with a known domain; split or classify the unknown path before admit"
		}
		// pure unknown: multi-package check is done at admission via paths
	}
	// Classic d791 failure mode: usage/schema foundation + API + UI polish.
	if has[domainUI] && (has[domainSchema] || has[domainUsage]) && (has[domainAPI] || has[domainDeploy] || has[domainParsing]) {
		return "composite_slice: UI polish must not share a Worker with schema/usage and API/deploy/pipeline work; admit one domain at a time"
	}
	if has[domainSchema] && has[domainUI] {
		return "composite_slice: schema/migration and UI are separate verifiable slices"
	}
	if has[domainAPI] && has[domainUI] {
		return "composite_slice: API and UI are separate verifiable slices"
	}
	// Data-plane usage + UI still mixed → reject. Pure public UI no longer gets
	// domainUsage from the word "usage" alone.
	if has[domainUsage] && has[domainUI] {
		return "composite_slice: usage data-plane and UI are separate verifiable slices"
	}
	if has[domainUsage] && has[domainAPI] {
		return "composite_slice: usage data-plane and API are separate verifiable slices"
	}
	if has[domainDeploy] && (has[domainUI] || has[domainSchema] || has[domainUsage] || has[domainAPI]) {
		return "composite_slice: deploy is a separate gate after implementation slices pass"
	}
	return ""
}

// compositeSliceReasonForInput applies domain composite rules plus unknown-only
// multi-package structural reject.
func compositeSliceReasonForInput(in WorkerStartInput) string {
	domains := detectSliceDomains(in)
	if reason := compositeSliceReason(domains); reason != "" {
		return reason
	}
	// Pure unknown spanning multiple structural packages → reject.
	onlyUnknown := len(domains) == 1 && domains[0] == domainUnknown
	if onlyUnknown && !unknownOnlySamePackage(in.Paths) {
		return "composite_slice: uncategorized paths span multiple top-level packages; split into one package per Worker"
	}
	return ""
}

func containsAnyFold(text string, needles ...string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(text, strings.ToLower(n)) {
			return true
		}
	}
	return false
}

package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	verifierAvailable          = "available"
	verifierAvailableFallback  = "available_fallback"
	verifierUnavailable        = "unavailable"
	verifierResolutionRequired = "resolution_required"
	verifierSetupRequired      = "setup_required"
)

// VerifierPreflight is a zero-model, side-effect-free launch check. It does not
// run the verifier: it only attests that the executable exists and, for Python
// `-m` commands, that the requested module is importable. This prevents Root
// from spending separate tool/model rounds discovering an unavailable runner.
type VerifierPreflight struct {
	Status       string `json:"status"`
	Command      string `json:"command,omitempty"`
	Source       string `json:"source,omitempty"`
	SetupCommand string `json:"setup_command,omitempty"`
	SetupAllowed bool   `json:"setup_allowed"`
	Reason       string `json:"reason"`
}

func verifierRunnable(status string) bool {
	return status == verifierAvailable || status == verifierAvailableFallback
}

// resolveProjectVerifier applies the durable project contract in priority
// order: an explicit harness/project command, the route's requested command,
// then a high-confidence repository-native runner. It never installs anything.
func resolveProjectVerifier(ctx context.Context, root, target string) VerifierPreflight {
	if explicit := strings.TrimSpace(os.Getenv("CLAUDEX_PROJECT_VERIFIER")); explicit != "" {
		out := preflightRootVerifier(ctx, root, explicit)
		out.Source = "explicit_environment"
		if !verifierRunnable(out.Status) {
			out.Reason = "explicit CLAUDEX_PROJECT_VERIFIER is not executable; fix the project/harness contract instead of guessing a fallback"
		}
		return out
	}

	requested := preflightRootVerifier(ctx, root, target)
	requested.Source = "route_request"
	if verifierRunnable(requested.Status) {
		return requested
	}

	if discovered, ok := discoverProjectVerifier(ctx, root); ok {
		if verifierRunnable(discovered.Status) {
			discovered.Status = verifierAvailableFallback
			discovered.Reason = "requested verifier was unavailable; repository metadata attested this executable project verifier"
		}
		return discovered
	}
	return requested
}

type packageManifest struct {
	Scripts map[string]string `json:"scripts"`
}

var makeTestTarget = regexp.MustCompile(`(?m)^test\s*:`)

func discoverProjectVerifier(ctx context.Context, root string) (VerifierPreflight, bool) {
	// Existing local Python environment is preferred over any setup proposal.
	for _, python := range []string{".venv/bin/python", "venv/bin/python"} {
		if fileExists(filepath.Join(root, python)) {
			candidate := preflightRootVerifier(ctx, root, python+" -m pytest -q")
			candidate.Source = "existing_python_environment"
			if verifierRunnable(candidate.Status) {
				return candidate, true
			}
		}
	}

	for _, candidate := range []struct {
		marker  string
		command string
		source  string
	}{
		{"go.mod", "go test ./...", "go.mod"},
		{"Cargo.toml", "cargo test", "Cargo.toml"},
		{"gradlew", "./gradlew test", "Gradle wrapper"},
		{"mvnw", "./mvnw test", "Maven wrapper"},
		{"tox.ini", "tox", "tox.ini"},
		{"pytest.ini", "pytest -q", "pytest.ini"},
	} {
		if !fileExists(filepath.Join(root, candidate.marker)) {
			continue
		}
		out := preflightRootVerifier(ctx, root, candidate.command)
		out.Source = candidate.source
		if verifierRunnable(out.Status) {
			return out, true
		}
	}

	if raw, err := os.ReadFile(filepath.Join(root, "Makefile")); err == nil && makeTestTarget.Match(raw) {
		out := preflightRootVerifier(ctx, root, "make test")
		out.Source = "Makefile:test"
		if verifierRunnable(out.Status) {
			return out, true
		}
	}

	if raw, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
		var manifest packageManifest
		if json.Unmarshal(raw, &manifest) == nil && strings.TrimSpace(manifest.Scripts["test"]) != "" {
			manager, setup := nodePackageManager(root)
			command := manager + " test"
			if dirExists(filepath.Join(root, "node_modules")) {
				out := preflightRootVerifier(ctx, root, command)
				out.Source = "package.json:scripts.test"
				if verifierRunnable(out.Status) {
					return out, true
				}
			}
			return VerifierPreflight{
				Status: verifierSetupRequired, Command: command, Source: "package.json:scripts.test",
				SetupCommand: setup, SetupAllowed: false,
				Reason: "project declares a test script but dependencies are not installed; setup is reported, never executed automatically",
			}, true
		}
	}

	if fileExists(filepath.Join(root, "uv.lock")) && fileContainsAny(filepath.Join(root, "pyproject.toml"), "pytest") {
		if _, ok := resolveVerifierExecutable(root, "uv"); ok {
			return VerifierPreflight{
				Status: verifierSetupRequired, Command: "uv run pytest -q", Source: "uv.lock",
				SetupCommand: "uv sync --frozen", SetupAllowed: false,
				Reason: "locked Python test dependency requires setup; report cold setup separately and never run it implicitly",
			}, true
		}
	}
	return VerifierPreflight{}, false
}

func nodePackageManager(root string) (manager, setup string) {
	switch {
	case fileExists(filepath.Join(root, "pnpm-lock.yaml")):
		return "pnpm", "pnpm install --frozen-lockfile"
	case fileExists(filepath.Join(root, "yarn.lock")):
		return "yarn", "yarn install --immutable"
	default:
		return "npm", "npm ci"
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileContainsAny(path string, values ...string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(raw))
	for _, value := range values {
		if strings.Contains(lower, strings.ToLower(value)) {
			return true
		}
	}
	return false
}

func preflightRootVerifier(ctx context.Context, root, target string) VerifierPreflight {
	command, exact := exactWorkerVerifier(target)
	if !exact {
		return VerifierPreflight{
			Status: verifierResolutionRequired,
			Reason: "verification target is not one literal executable command; resolve one repository-supported command before final verification",
		}
	}

	fields := strings.Fields(command)
	env := append([]string(nil), os.Environ()...)
	i := 0
	for i < len(fields) && shellAssignmentPattern.MatchString(fields[i]) {
		env = append(env, fields[i])
		i++
	}
	if i >= len(fields) {
		return VerifierPreflight{Status: verifierResolutionRequired, Reason: "verifier has no executable"}
	}
	executable, args := fields[i], fields[i+1:]
	resolved, ok := resolveVerifierExecutable(root, executable)
	fallback := false
	if !ok && filepath.Base(executable) == "python" {
		if candidate, lookupErr := exec.LookPath("python3"); lookupErr == nil {
			resolved, ok, fallback = candidate, true, true
			executable = "python3"
			fields[i] = executable
			command = strings.Join(fields, " ")
		}
	}
	if !ok {
		return VerifierPreflight{
			Status: verifierUnavailable, Command: command,
			Reason: "verifier executable is unavailable; do not trial or substitute it in Root",
		}
	}
	if module := pythonModule(args, filepath.Base(executable)); module != "" {
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		probe := exec.CommandContext(probeCtx, resolved, "-c", "import importlib.util,sys; sys.exit(0 if importlib.util.find_spec(sys.argv[1]) else 1)", module)
		probe.Dir = root
		probe.Env = env
		if err := probe.Run(); err != nil {
			return VerifierPreflight{
				Status: verifierUnavailable, Command: command,
				Reason: "verifier module is unavailable; do not install dependencies or retry interpreters in Root",
			}
		}
	}
	status := verifierAvailable
	reason := "verifier executable is available; run this exact command once after integration"
	if fallback {
		status = verifierAvailableFallback
		reason = "python alias was unavailable; preflight attested the displayed python3 equivalent and its module"
	}
	return VerifierPreflight{Status: status, Command: command, Reason: reason}
}

func resolveVerifierExecutable(root, executable string) (string, bool) {
	if strings.ContainsAny(executable, `/\\`) {
		path := executable
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		info, err := os.Stat(path)
		return path, err == nil && !info.IsDir() && info.Mode()&0o111 != 0
	}
	path, err := exec.LookPath(executable)
	return path, err == nil
}

func pythonModule(args []string, executable string) string {
	if executable != "python" && executable != "python3" && !strings.HasPrefix(executable, "python3.") {
		return ""
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-m" {
			return strings.Trim(args[i+1], `"'`)
		}
	}
	return ""
}

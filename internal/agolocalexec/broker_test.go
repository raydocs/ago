package agolocalexec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func brokerTestPlan(t *testing.T) LaunchPlan {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("canonicalize test root: %v", err)
	}
	plan := LaunchPlan{Origin: "model:test", Executable: "/usr/bin/printf", Arguments: []string{"hi"}, WorkingDir: root, Environment: map[string]string{"LANG": "C", "Z": "last"}, ReadRoots: []string{"/usr/bin", root}, WriteRoots: []string{root}, SyntheticHome: filepath.Join(root, "home"), SyntheticTemp: filepath.Join(root, "tmp"), ProfileID: "ago.model.v1", Network: NetworkDisabled, Deadline: time.Second, Output: OutputBudget{HeadBytes: 4, TailBytes: 3}, ApprovalNonce: "nonce"}
	bound, err := BindSeatbeltProfile(plan)
	if err != nil {
		t.Fatalf("bind Seatbelt profile: %v", err)
	}
	return bound
}

func TestRenderSeatbeltProfileIsDenyDefaultAndEscapesPaths(t *testing.T) {
	p := brokerTestPlan(t)
	p.ReadRoots = append(p.ReadRoots, filepath.Join(p.WorkingDir, `a"b`))
	profile, err := RenderSeatbeltProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"(deny default)", "(deny network*)", `(allow file-read-data (literal "/"))`, `(allow file-read* (subpath (param "AGO_READ_000")))`, `(allow file-write* (subpath (param "AGO_WRITE_000")))`} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile missing %q:\n%s", want, profile)
		}
	}
	if strings.Contains(profile, p.WorkingDir) || strings.Contains(profile, `a"b`) {
		t.Fatal("parameter value leaked into profile text")
	}
}

func TestSandboxExecArgvAndEnvironmentAreExact(t *testing.T) {
	p := brokerTestPlan(t)
	argv, err := SandboxExecArgv(p, "/private/profile.sb")
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) < 9 || !reflect.DeepEqual(argv[:2], []string{"/usr/bin/sandbox-exec", "-D"}) || !strings.HasPrefix(argv[2], "AGO_READ_000=") || argv[len(argv)-2] != p.Executable || argv[len(argv)-1] != "hi" {
		t.Fatalf("argv %#v", argv)
	}
	env, err := LaunchEnvironment(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(env, []string{"HOME=" + p.SyntheticHome, "LANG=C", "TMPDIR=" + p.SyntheticTemp, "Z=last"}) {
		t.Fatalf("env %#v", env)
	}
}

func TestExecuteBrokerUsesPipeWireContract(t *testing.T) {
	p := brokerTestPlan(t)
	digest, _ := p.Digest()
	script := filepath.Join(t.TempDir(), "supervisor")
	body := `#!/bin/sh
IFS= read -r line
printf '{"version":2,"digest":"%s","exit_code":7,"stdout":{"head":"aGk=","tail":"","dropped_bytes":0,"total_bytes":2},"stderr":{"head":"","tail":"","dropped_bytes":0,"total_bytes":0}}\n' '` + digest + `'
`
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	r, err := ExecuteBroker(context.Background(), script, p)
	if err != nil {
		t.Fatal(err)
	}
	if r.ExitCode != 7 || string(r.Stdout.Head) != "hi" {
		t.Fatalf("result %+v", r)
	}
}

func TestExecuteBrokerRejectsDigestMismatch(t *testing.T) {
	p := brokerTestPlan(t)
	script := filepath.Join(t.TempDir(), "supervisor")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nread x\nprintf '{\"version\":2,\"digest\":\"wrong\",\"exit_code\":0,\"stdout\":{},\"stderr\":{}}\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ExecuteBroker(context.Background(), script, p); err == nil {
		t.Fatal("accepted mismatched digest")
	}
}

func TestExecuteBrokerClosesLivenessPipeOnCancellation(t *testing.T) {
	p := brokerTestPlan(t)
	p.Deadline = 10 * time.Second
	script := filepath.Join(t.TempDir(), "supervisor")
	// FD 3 is the inherited liveness pipe. The fake supervisor exits only when
	// the broker closes it, making this test fail if cancellation kills/bypasses
	// the supervisor instead of using the ownership protocol.
	body := "#!/bin/sh\ncat <&3 >/dev/null\nexit 23\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	started := time.Now()
	if _, err := ExecuteBroker(ctx, script, p); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("ExecuteBroker error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("liveness cancellation took %v", elapsed)
	}
}

func TestLaunchEnvelopeJSONIsDeterministic(t *testing.T) {
	p := brokerTestPlan(t)
	a, _ := NewLaunchEnvelope(p)
	b, _ := NewLaunchEnvelope(p)
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) != string(bj) {
		t.Fatal("nondeterministic JSON")
	}
}

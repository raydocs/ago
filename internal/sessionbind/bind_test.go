package sessionbind

import (
	"path/filepath"
	"testing"
)

func TestRecordAndResolveByClaudePIDAndCWD(t *testing.T) {
	t.Setenv("CLAUDEX_SESSION_BINDING_DIR", t.TempDir())
	cwd := t.TempDir()
	if err := Record(4242, "root-session", cwd); err != nil {
		t.Fatal(err)
	}
	binding, ok := Resolve(4242, cwd)
	if !ok || binding.SessionID != "root-session" || binding.ClaudePID != 4242 {
		t.Fatalf("binding not resolved: %#v ok=%t", binding, ok)
	}
	if _, ok := Resolve(4242, filepath.Join(cwd, "other")); ok {
		t.Fatal("binding crossed workspace boundary")
	}
	if _, ok := Resolve(9999, cwd); ok {
		t.Fatal("binding crossed Claude process boundary")
	}
}

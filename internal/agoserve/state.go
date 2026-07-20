package agoserve

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file is about one command that deletes things.
//
// `demo --reset` started life as os.RemoveAll(state) with a user-supplied
// --state. A typo — or a shell variable that expanded to empty, or a path with
// a trailing component the user did not expect — would recursively delete a
// directory Ago never created. That is not a bug to be careful around; it is a
// capability that must not exist.
//
// So reset does not delete a directory. It deletes the specific entries Ago
// creates, inside a directory Ago can prove it created, and only after every
// preflight has passed. Anything it did not create it leaves alone, including
// in the success path: a user who kept a note in that directory keeps the note.

// markerName is the file Ago writes when it creates a demo state directory.
// Its presence — with the right contents — is what "Ago owns this" means.
const markerName = ".ago-demo-state"

// markerMagic is checked verbatim. A directory that merely has a file with the
// right name is not owned; the contents have to say so.
const markerMagic = "ago-demo-state-v1"

type marker struct {
	Magic     string    `json:"magic"`
	CreatedAt time.Time `json:"created_at"`
}

// demoEntries is everything the demo creates inside its state directory. Reset
// removes exactly these, so an unrecognised entry is never at risk.
var demoEntries = []string{
	"ago.db", "ago.db-wal", "ago.db-shm",
	"greeter", "artifacts", "worktrees", "integration",
}

// ClaimState makes a directory Ago's, or refuses to use it at all.
//
// This is where the whole ownership model is actually decided, and the first
// version of it was wrong in a way that mattered. It wrote the marker whenever
// the sample repository was missing — which is true of every directory a user
// might point --state at. Two ordinary commands then destroyed real data:
//
//	ago demo --state ~/myproject          # planted a marker
//	ago demo --state ~/myproject --reset  # deleted myproject/artifacts
//
// "artifacts" and "integration" are ordinary names. Narrowing the delete to
// Ago's own entries was not enough, because the authorisation was free.
//
// A directory is claimable only when using it cannot be a mistake:
//
//   - it does not exist, so Ago creates it;
//   - it is empty;
//   - it already carries Ago's marker;
//   - or everything in it is one of Ago's own entries, which is what state
//     written by a build from before markers existed looks like. Adopting it
//     is what keeps that state resettable instead of stranding it.
//
// Anything else is refused, and the demo does not run there.
func ClaimState(state string) error {
	if strings.TrimSpace(state) == "" {
		return fmt.Errorf("--state 不能为空")
	}
	entries, err := os.ReadDir(state)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(state, 0o700); err != nil {
			return fmt.Errorf("准备演示目录 %s：%w", state, err)
		}
		return WriteMarker(state)
	}
	if err != nil {
		return fmt.Errorf("读取演示目录 %s：%w", state, err)
	}
	if OwnsState(state) {
		return nil
	}
	for _, entry := range entries {
		if !agoOwnedEntry(entry.Name()) {
			return fmt.Errorf(
				"拒绝把 %s 当作演示目录：里面已经有不是 Ago 创建的内容（例如 %s）。"+
					"请换一个空目录或不存在的路径 —— Ago 只会认领自己创建的目录，"+
					"否则 --reset 可能删掉你自己的 artifacts/ 或 integration/",
				state, entry.Name())
		}
	}
	// Recognisably Ago's, so it is adopted and becomes resettable.
	return WriteMarker(state)
}

func agoOwnedEntry(name string) bool {
	if name == markerName {
		return true
	}
	for _, entry := range demoEntries {
		if name == entry {
			return true
		}
	}
	return false
}

// WriteMarker claims a directory for the demo. Callers go through ClaimState,
// which decides whether claiming is legitimate; this only writes the file.
func WriteMarker(state string) error {
	encoded, err := json.Marshal(marker{Magic: markerMagic, CreatedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(state, markerName), append(encoded, '\n'), 0o600)
}

// OwnsState reports whether the directory carries Ago's marker. It is a
// question about ownership, not about existence: a directory the user made
// themselves and pointed --state at answers false.
func OwnsState(state string) bool {
	return checkMarker(state) == nil
}

func checkMarker(state string) error {
	path := filepath.Join(state, markerName)
	// Lstat, so a symlink named like the marker cannot vouch for a directory
	// by pointing at a real marker somewhere else.
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("目录 %s 里没有 Ago 的归属标记（%s）", state, markerName)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("归属标记 %s 不是普通文件", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("无法读取归属标记 %s：%w", path, err)
	}
	var decoded marker
	if err := json.Unmarshal(content, &decoded); err != nil || decoded.Magic != markerMagic {
		return fmt.Errorf("归属标记 %s 的内容不是 Ago 写的", path)
	}
	return nil
}

// ResetState removes the demo's own state from a directory Ago can prove it
// owns.
//
// It refuses, rather than deletes, whenever it cannot prove that. The proof has
// four parts and all of them must hold:
//
//  1. --state itself is not a symlink, and every removal is resolved against
//     the canonical directory. A symlinked --state is refused outright: the
//     user named one directory and the delete would land in another. Symlinked
//     ANCESTORS are resolved rather than refused, because refusing them would
//     refuse most real machines — on macOS /var is a link to /private/var, and
//     a home directory is often reached through one. Resolving is safe: every
//     removal below happens inside the canonical directory, by fixed name,
//     with links unlinked instead of followed.
//  2. The path is not one of a short list that is never a demo directory: the
//     filesystem root, the user's home, an ancestor of the home, a directory
//     shallower than two segments, or a git repository.
//  3. Ago's marker is present, is a regular file, and says what Ago writes.
//  4. Only the entries the demo creates are removed. Everything else in the
//     directory survives, in this path and in every refusal path.
func ResetState(state, home string) error {
	resolved, err := CheckResetAllowed(state, home)
	if err != nil {
		return err
	}
	for _, entry := range demoEntries {
		target := filepath.Join(resolved, entry)
		// Lstat first: a symlink placed at one of these names is unlinked, not
		// followed, so a hostile link cannot turn a delete of "artifacts" into
		// a delete of somewhere else.
		info, err := os.Lstat(target)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("检查 %s：%w", target, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(target); err != nil {
				return fmt.Errorf("删除符号链接 %s：%w", target, err)
			}
			continue
		}
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("清理 %s：%w", target, err)
		}
	}
	// The marker stays. The directory was Ago's before the reset and still is,
	// so removing it would only make the next run re-decide the question — and
	// in a directory that also holds something of the user's, that re-decision
	// would refuse, leaving --reset as a way to make the demo unusable.
	return nil
}

// CheckResetAllowed answers "may --reset touch this directory?" and deletes
// nothing.
//
// It is separate from the delete so the answer can be obtained early — before
// any preflight has created or probed anything — and reported in the words of
// the refusal rather than as whatever the next step happened to fail on. The
// actual removal still runs only after every other preflight has passed.
func CheckResetAllowed(state, home string) (string, error) {
	resolved, err := safeStatePath(state)
	if err != nil {
		return "", err
	}
	if err := refuseHighRiskPath(resolved, home); err != nil {
		return "", err
	}
	if err := checkMarker(resolved); err != nil {
		return "", fmt.Errorf("拒绝重置 %s：%w。--reset 只会清理 Ago 自己创建的演示目录", state, err)
	}
	return resolved, nil
}

// safeStatePath refuses a symlinked --state and canonicalises everything else.
//
// The distinction is the whole point. A symlinked --state is a mismatch between
// what the user named and what would be deleted, so it is refused. A symlinked
// ancestor is not a mismatch — it is how the machine is laid out — so it is
// resolved, and every subsequent operation happens on the canonical path.
//
// A path-prefix check would not do this job at all: a symlink inside a
// directory with the right prefix still points wherever it likes. What makes
// the removals safe is that they are fixed names inside the canonical
// directory, each unlinked rather than followed.
func safeStatePath(state string) (string, error) {
	if strings.TrimSpace(state) == "" {
		return "", fmt.Errorf("--state 不能为空")
	}
	absolute, err := filepath.Abs(state)
	if err != nil {
		return "", fmt.Errorf("解析 --state %q：%w", state, err)
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("目录 %s 不存在，没有什么可以重置", absolute)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("拒绝重置 %s：它是一个符号链接", absolute)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("拒绝重置 %s：它不是目录", absolute)
	}
	// Everything after this point works on the canonical path, so no later
	// step can be redirected by a link in an ancestor directory.
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("解析 %s 的真实路径：%w", absolute, err)
	}
	return filepath.Clean(resolved), nil
}

// refuseHighRiskPath rejects locations that are never a demo state directory,
// whatever a marker might claim.
//
// The marker check alone would already stop these, because Ago never wrote a
// marker into any of them. This list exists so that a mistake in the marker
// logic — or a marker a user copied around — still cannot reach them.
func refuseHighRiskPath(resolved, home string) error {
	if resolved == string(filepath.Separator) {
		return fmt.Errorf("拒绝重置文件系统根目录")
	}
	// Depth is measured below the volume, so C:\demo counts as one segment on
	// Windows exactly as /demo does elsewhere.
	below := strings.TrimPrefix(resolved, filepath.VolumeName(resolved))
	if segments := strings.Split(strings.Trim(filepath.ToSlash(below), "/"), "/"); len(segments) < 2 || segments[0] == "" {
		return fmt.Errorf("拒绝重置 %s：路径太靠近根目录，不可能是演示目录", resolved)
	}
	// Both the resolved and the literal home are checked. If the home cannot
	// be resolved — it is missing, or unreadable — falling back to only one of
	// them would let the comparison silently miss.
	for _, candidate := range homeCandidates(home) {
		if resolved == candidate {
			return fmt.Errorf("拒绝重置 %s：那是你的主目录", resolved)
		}
		if isAncestor(resolved, candidate) {
			return fmt.Errorf("拒绝重置 %s：它包含你的主目录", resolved)
		}
	}
	// A worktree or a submodule has .git as a FILE holding a gitdir: pointer,
	// so requiring a directory here missed both.
	if _, err := os.Lstat(filepath.Join(resolved, ".git")); err == nil {
		return fmt.Errorf("拒绝重置 %s：那是一个 git 仓库（或 worktree/submodule）", resolved)
	}
	return nil
}

func homeCandidates(home string) []string {
	if strings.TrimSpace(home) == "" {
		return nil
	}
	candidates := []string{filepath.Clean(home)}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		if cleaned := filepath.Clean(resolved); cleaned != candidates[0] {
			candidates = append(candidates, cleaned)
		}
	}
	return candidates
}

// isAncestor reports whether parent contains child, matching on path segments
// so /home/ago does not appear to contain /home/agostino.
func isAncestor(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if parent == child {
		return false
	}
	prefix := parent
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(child, prefix)
}

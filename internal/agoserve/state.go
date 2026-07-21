package agoserve

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file is about one command that deletes things, and it has been wrong
// three times. Each time the same way: the authority to delete was granted on
// evidence that did not establish who created anything.
//
//	v1  wrote the marker whenever the sample repository was absent — true of
//	    any directory. `--state ~/myproject` then `--reset` deleted
//	    ~/myproject/artifacts.
//	v2  additionally adopted any directory whose entries all carried Ago's
//	    names. A directory holding only artifacts/report.pdf is an ordinary
//	    project directory. A name is not provenance.
//	v3  claimed only absent or empty directories — correct at the moment of
//	    the claim, and then never re-decided. Point --state at ./build before
//	    it exists, let `make` fill it, and --reset removed build/artifacts.
//
// What all three share is a claim that outlives the fact it was based on. So
// the marker is no longer a flag saying "this directory is Ago's". It is a
// binding, and it is rechecked against the filesystem every time it is used:
//
//   - to the directory, by device and inode, so a copy, a move, a restore, or
//     a symlink pointing somewhere else is not the thing that was claimed;
//   - to each entry Ago created, by device and inode, so anything that
//     appeared under one of Ago's names without Ago creating it is not Ago's
//     to remove.
//
// The result is that reset deletes what Ago made, and can tell the difference.

// markerName is the file that records the binding. Its presence proves nothing
// on its own; its contents have to still match the filesystem.
const markerName = ".ago-demo-state"

// markerMagic is checked verbatim, so a file that merely has the right name is
// not a marker.
const markerMagic = "ago-demo-state-v2"

// entryID is the filesystem's identity for one thing Ago created.
type entryID struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

func (id entryID) known() bool { return id.Device != 0 || id.Inode != 0 }

func identityOf(path string) (entryID, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return entryID{}, false
	}
	device, inode, ok := fileIdentity(info)
	if !ok {
		return entryID{}, false
	}
	return entryID{Device: device, Inode: inode}, true
}

type marker struct {
	Magic     string    `json:"magic"`
	CreatedAt time.Time `json:"created_at"`
	// Path is the canonical directory this marker was written for. A marker
	// found anywhere else is describing something that is not here.
	Path string `json:"path"`
	// Directory is that directory's own identity, which a copy does not carry.
	Directory entryID `json:"directory"`
	// Entries records what Ago actually created, by identity. Reset removes an
	// entry only when what is on disk now is still the same object.
	Entries map[string]entryID `json:"entries,omitempty"`
}

// reservedEntries are the names Ago's own components use inside a state
// directory. Being on this list is what makes an entry a CANDIDATE for
// removal; the recorded identity is what makes it removable.
var reservedEntries = []string{
	"ago.db", "ago.db-wal", "ago.db-shm",
	"greeter", "artifacts", "worktrees", "integration",
}

// ClaimState makes a directory Ago's, or refuses to use it at all, and returns
// the canonical path everything afterwards should use.
//
// A directory is claimable in exactly three cases, and each one means either
// Ago creates the contents or there are no contents to mistake:
//
//   - the path does not exist, so Ago creates it;
//   - it exists and is genuinely empty;
//   - it already carries a marker that still binds to it.
//
// Anything else is refused. Callers ask CanClaim first, which answers the same
// question without touching anything.
func ClaimState(state string) (string, error) {
	resolved, err := CanClaim(state)
	if err != nil {
		return "", err
	}
	if _, statErr := os.Lstat(resolved); os.IsNotExist(statErr) {
		if err := os.MkdirAll(resolved, 0o700); err != nil {
			return "", fmt.Errorf("准备演示目录 %s：%w", resolved, err)
		}
	}
	// Clear any debris from an interrupted claim before writing a new marker,
	// so the directory does not accumulate them.
	removeMarkerTemporaries(resolved)
	if OwnsState(resolved) {
		return resolved, nil
	}
	if err := writeMarker(resolved, nil); err != nil {
		return "", err
	}
	return resolved, nil
}

// markerTemporaryPrefix is the pattern writeMarker creates before renaming.
const markerTemporaryPrefix = markerName + "-"

func isMarkerTemporary(name string) bool {
	return strings.HasPrefix(name, markerTemporaryPrefix)
}

func removeMarkerTemporaries(state string) {
	entries, err := os.ReadDir(state)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if isMarkerTemporary(entry.Name()) {
			_ = os.Remove(filepath.Join(state, entry.Name()))
		}
	}
}

// CanClaim answers "may Ago use this directory?" and changes nothing.
//
// It is separate so the answer can be obtained before any preflight has
// created, probed, or written anything: a directory Ago is going to refuse
// must not first have a temporary file written into it.
func CanClaim(state string) (string, error) {
	resolved, err := statePath(state)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(resolved)
	if os.IsNotExist(err) {
		return resolved, nil
	}
	if err != nil {
		return "", fmt.Errorf("读取演示目录 %s：%w", resolved, err)
	}
	if OwnsState(resolved) {
		return resolved, nil
	}
	// Ago's own half-written markers do not count against emptiness. A crash
	// between writing the temporary file and renaming it into place would
	// otherwise leave a hidden file that makes the directory permanently
	// unclaimable, and the user would have to find and delete something they
	// never knew existed.
	remaining := 0
	var first string
	for _, entry := range entries {
		if isMarkerTemporary(entry.Name()) {
			continue
		}
		if remaining == 0 {
			first = entry.Name()
		}
		remaining++
	}
	if remaining == 0 {
		return resolved, nil
	}
	return "", fmt.Errorf(
		"拒绝把 %s 当作演示目录：它不是空的，也没有 Ago 的归属标记（里面有 %s）。"+
			"Ago 只会使用自己创建的目录或一个空目录 —— 否则 --reset 可能删掉你自己的 "+
			"artifacts/、integration/ 或 worktrees/。请换一个空目录或一个还不存在的路径",
		resolved, first)
}

// PresentReservedEntries reports which of Ago's names already exist. A caller
// takes this before starting, so that whatever appears afterwards can be
// attributed to the run that created it.
func PresentReservedEntries(state string) map[string]bool {
	present := map[string]bool{}
	for _, name := range reservedEntries {
		if _, err := os.Lstat(filepath.Join(state, name)); err == nil {
			present[name] = true
		}
	}
	return present
}

// RecordCreatedEntries records the identity of everything Ago's own components
// just created, so a later reset can tell those objects apart from anything
// that later takes their name.
//
// Attribution is by absence: the caller reports which reserved names existed
// before it started, and only names that appeared since are recorded. That is
// what closes the case the previous version got wrong — a user who fills a
// claimed directory with their own artifacts/ never has it recorded, so reset
// leaves it alone.
func RecordCreatedEntries(state string, existedBefore map[string]bool) error {
	current, err := readMarker(state)
	if err != nil {
		// No usable marker means nothing to record against. Not worth failing
		// a running demo over; a later reset simply refuses.
		return nil
	}
	entries := current.Entries
	if entries == nil {
		entries = map[string]entryID{}
	}
	changed := false
	for _, name := range reservedEntries {
		if existedBefore[name] {
			// It was already there when this run began. If Ago recorded it on
			// an earlier run the record still stands; if not, Ago did not make
			// it and must not claim it now.
			continue
		}
		identity, ok := identityOf(filepath.Join(state, name))
		if !ok {
			continue
		}
		entries[name] = identity
		changed = true
	}
	if !changed {
		return nil
	}
	return writeMarker(state, entries)
}

func writeMarker(state string, entries map[string]entryID) error {
	directory, ok := identityOf(state)
	if !ok {
		return fmt.Errorf("无法读取 %s 的文件系统标识，不能安全地认领这个目录", state)
	}
	encoded, err := json.Marshal(marker{
		Magic:     markerMagic,
		CreatedAt: time.Now().UTC(),
		Path:      canonicalDir(state),
		Directory: directory,
		Entries:   entries,
	})
	if err != nil {
		return err
	}
	// Written whole and moved into place, so a crash mid-write cannot destroy
	// a valid marker and leave half of one. A torn marker would fail closed
	// anyway; this keeps a good one from being lost to an interrupted rewrite.
	temporary, err := os.CreateTemp(state, markerTemporaryPrefix+"*")
	if err != nil {
		return fmt.Errorf("写入归属标记：%w", err)
	}
	name := temporary.Name()
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		_ = temporary.Close()
		_ = os.Remove(name)
		return fmt.Errorf("写入归属标记：%w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("写入归属标记：%w", err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("写入归属标记：%w", err)
	}
	if err := os.Rename(name, filepath.Join(state, markerName)); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("写入归属标记：%w", err)
	}
	return nil
}

// WriteMarker claims a directory outright. It exists for tests that need a
// marker without going through a claim; ordinary callers use ClaimState.
func WriteMarker(state string) error { return writeMarker(state, nil) }

// OwnsState reports whether the marker in this directory still binds to it.
func OwnsState(state string) bool {
	_, err := readMarker(state)
	return err == nil
}

// readMarker reads the marker and checks that it still describes THIS
// directory. Every failure is an error: a marker that cannot be read, parsed,
// or matched grants nothing.
func readMarker(state string) (marker, error) {
	path := filepath.Join(state, markerName)
	// Lstat, so a symlink named like the marker cannot vouch for a directory
	// by pointing at a real marker somewhere else.
	info, err := os.Lstat(path)
	if err != nil {
		return marker{}, fmt.Errorf("目录 %s 里没有 Ago 的归属标记（%s）", state, markerName)
	}
	if !info.Mode().IsRegular() {
		return marker{}, fmt.Errorf("归属标记 %s 不是普通文件", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return marker{}, fmt.Errorf("无法读取归属标记 %s：%w", path, err)
	}
	var decoded marker
	if err := json.Unmarshal(content, &decoded); err != nil || decoded.Magic != markerMagic {
		return marker{}, fmt.Errorf("归属标记 %s 的内容不是 Ago 写的", path)
	}
	// The binding. A marker that travelled here — copied, moved, restored from
	// a backup, extracted from an archive — describes a directory that is not
	// this one, and authorises nothing.
	if decoded.Path != canonicalDir(state) {
		return marker{}, fmt.Errorf(
			"归属标记 %s 记录的是另一个目录（%s），可能是被复制或移动过来的，不能用来授权删除",
			path, decoded.Path)
	}
	identity, ok := identityOf(state)
	if !ok {
		return marker{}, fmt.Errorf("无法读取 %s 的文件系统标识", state)
	}
	if !decoded.Directory.known() || decoded.Directory != identity {
		return marker{}, fmt.Errorf(
			"归属标记 %s 记录的目录已经不是现在这一个（可能被删除后重建，或从备份恢复），不能用来授权删除", path)
	}
	return decoded, nil
}

// canonicalDir is the form the marker records and compares against, so a
// directory reached by two spellings — through a symlinked ancestor, or with a
// different relative path — is still recognised as the same one. A path that
// cannot be resolved falls back to its cleaned form, which fails closed: an
// unresolvable path will simply not match what was recorded.
func canonicalDir(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

// ResetState removes what Ago created, in a directory Ago can still prove it
// owns, and nothing else.
//
// The proof has four parts and all of them must hold:
//
//  1. --state itself is not a symlink, and everything works on the canonical
//     directory. A symlinked --state is a mismatch between what the user named
//     and what would be touched. Symlinked ANCESTORS are resolved rather than
//     refused, because refusing them would refuse most real machines — on
//     macOS /var is a link to /private/var.
//  2. The path is not one of a short list that is never a demo directory: the
//     filesystem root, the user's home, an ancestor of the home, a directory
//     shallower than two segments, or a git repository.
//  3. The marker is present, well-formed, and still binds to this directory by
//     path and by device and inode.
//  4. Each entry is removed only if its recorded identity still matches what
//     is on disk. Everything else survives — including a directory that took
//     one of Ago's names after Ago's own was gone.
func ResetState(state, home string) error {
	resolved, err := CheckResetAllowed(state, home)
	if err != nil {
		return err
	}
	recorded, err := readMarker(resolved)
	if err != nil {
		return err
	}
	for name, identity := range recorded.Entries {
		if !isReservedEntry(name) || !identity.known() {
			continue
		}
		target := filepath.Join(resolved, name)
		current, ok := identityOf(target)
		if !ok {
			continue
		}
		if current != identity {
			// Something else is here under Ago's name. It is not what was
			// recorded, so it is not Ago's to remove.
			continue
		}
		info, err := os.Lstat(target)
		if err != nil {
			continue
		}
		// A symlink is unlinked, not followed, so a link swapped in for a
		// recorded directory cannot redirect the delete.
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
	// The record is cleared, not the marker: the directory is still Ago's, so
	// the next run need not re-decide that — but nothing Ago no longer has is
	// recorded any more either.
	return writeMarker(resolved, nil)
}

func isReservedEntry(name string) bool {
	for _, entry := range reservedEntries {
		if name == entry {
			return true
		}
	}
	return false
}

// CheckResetAllowed answers "may --reset touch this directory?" and deletes
// nothing.
//
// It is separate from the delete so the answer can be obtained early — before
// any preflight has created or probed anything — and reported in the words of
// the refusal rather than as whatever the next step happened to fail on.
func CheckResetAllowed(state, home string) (string, error) {
	resolved, err := statePath(state)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(resolved); err != nil {
		return "", fmt.Errorf("目录 %s 不存在，没有什么可以重置", resolved)
	}
	if err := refuseHighRiskPath(resolved, home); err != nil {
		return "", err
	}
	if _, err := readMarker(resolved); err != nil {
		return "", fmt.Errorf("拒绝重置 %s：%w。--reset 只会清理 Ago 自己创建的演示目录", state, err)
	}
	return resolved, nil
}

// statePath refuses a symlinked --state and canonicalises everything else.
//
// The distinction is the whole point. A symlinked --state is a mismatch
// between what the user named and what would be touched, so it is refused —
// and it is refused HERE, on the claiming side as well as the reset side,
// because a claim that lands in a directory the user never named is how the
// two sides come to disagree about which directory they are discussing.
//
// A symlinked ancestor is not a mismatch — it is how the machine is laid out —
// so it is resolved, and everything afterwards happens on the canonical path.
//
// A path-prefix check would not do this job at all: a symlink inside a
// directory with the right prefix still points wherever it likes. What makes
// the removals safe is that they are recorded names inside the canonical
// directory, matched by identity, each unlinked rather than followed.
func statePath(state string) (string, error) {
	if strings.TrimSpace(state) == "" {
		return "", fmt.Errorf("--state 不能为空")
	}
	absolute, err := filepath.Abs(state)
	if err != nil {
		return "", fmt.Errorf("解析 --state %q：%w", state, err)
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Lstat(absolute)
	if os.IsNotExist(err) {
		// It does not exist yet, so there is no link here to follow. The
		// existing part is canonicalised so that this claim and a later reset
		// of the same argument name the same directory.
		return canonicalisePrefix(absolute)
	}
	if err != nil {
		return "", fmt.Errorf("检查 %s：%w", absolute, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("拒绝把 %s 当作演示目录：它是一个符号链接，指向的目录和你写的不是同一个", absolute)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("拒绝把 %s 当作演示目录：它不是目录", absolute)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("解析 %s 的真实路径：%w", absolute, err)
	}
	return filepath.Clean(resolved), nil
}

// canonicalisePrefix resolves the part of a not-yet-existing path that does
// exist, so `--state ~/x/y` and a later reset of the same argument agree on
// one directory even when the home is reached through a link.
func canonicalisePrefix(absolute string) (string, error) {
	var missing []string
	current := absolute
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", fmt.Errorf("解析 %s 的真实路径：%w", current, err)
			}
			return filepath.Join(append([]string{filepath.Clean(resolved)}, reverse(missing)...)...), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return absolute, nil
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func reverse(values []string) []string {
	out := make([]string, len(values))
	for index, value := range values {
		out[len(values)-1-index] = value
	}
	return out
}

// refuseHighRiskPath rejects locations that are never a demo state directory,
// whatever a marker might claim.
//
// The marker check alone would already stop these, because Ago never wrote a
// marker into any of them. This list exists so that a mistake in the marker
// logic still cannot reach them.
func refuseHighRiskPath(resolved, home string) error {
	if resolved == string(filepath.Separator) {
		return fmt.Errorf("拒绝重置 %s：那是文件系统根目录", resolved)
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

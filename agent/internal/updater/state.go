package updater

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// StartState tracks whether the running build ever reached a healthy UI.
// A version that crashes twice before that point is rolled back.
type StartState struct {
	Version string    `json:"version"`
	Crashes int       `json:"crashes"`
	Healthy bool      `json:"healthy"`
	Updated time.Time `json:"updated_at"`
}

// RollbackNotice is written before launching the previous version so the
// (older) build can surface a persistent alert after it starts.
type RollbackNotice struct {
	From string    `json:"from"`
	To   string    `json:"to"`
	At   time.Time `json:"at"`
}

// BeginStart records a startup attempt for version and reports the version to
// fall back to when this build has now failed twice without reaching the UI.
func BeginStart(statePath, installRoot, version string) (string, error) {
	if err := CheckVersion(version); err != nil {
		return "", err
	}
	st := readStartState(statePath)
	switch {
	case st.Version != version:
		st = StartState{Version: version, Crashes: 1}
	case st.Healthy:
		// Last run was healthy; treat this as a fresh first attempt.
		st = StartState{Version: version, Crashes: 1}
	default:
		st.Crashes++
	}
	st.Updated = time.Now().UTC()
	if err := writeJSON(statePath, st); err != nil {
		return "", err
	}
	if st.Crashes < 2 {
		return "", nil
	}
	prev, err := PreviousVersion(installRoot, version)
	if err != nil {
		return "", err
	}
	return prev, nil
}

// MarkHealthy clears the crash counter once the UI is interactive.
func MarkHealthy(statePath, version string) error {
	st := readStartState(statePath)
	if st.Version != version {
		st = StartState{Version: version}
	}
	st.Crashes = 0
	st.Healthy = true
	st.Updated = time.Now().UTC()
	return writeJSON(statePath, st)
}

// WriteCurrent atomically points the launcher at version.
func WriteCurrent(installRoot, version string) error {
	if err := CheckVersion(version); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(installRoot, ".current-*.tmp")
	if err != nil {
		return fmt.Errorf("更新版本指针失败：%w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := io.WriteString(tmp, version+"\r\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(installRoot, "current.txt"))
}

// ReadCurrent returns the version named by current.txt.
func ReadCurrent(installRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(installRoot, "current.txt"))
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(data))
	if err := CheckVersion(v); err != nil {
		return "", fmt.Errorf("current.txt 内容无效：%w", err)
	}
	return v, nil
}

// InstallRoot derives the versioned layout (<root>/<version>/PWF Agent.exe)
// from an executable path. Development builds (for example build/bin) do not
// match a version directory, so update and rollback features stay off.
func InstallRoot(exePath string) (root, version string, ok bool) {
	versionDir := filepath.Dir(exePath)
	version = filepath.Base(versionDir)
	if CheckVersion(version) != nil {
		return "", "", false
	}
	root = filepath.Dir(versionDir)
	if root == versionDir || filepath.Dir(root) == root {
		return "", "", false
	}
	return root, version, true
}

// Target resolves current.txt to the installed executable, refusing paths
// outside installRoot.
func Target(installRoot string) (string, error) {
	version, err := ReadCurrent(installRoot)
	if err != nil {
		return "", err
	}
	target := filepath.Join(installRoot, version, "PWF Agent.exe")
	rel, err := filepath.Rel(installRoot, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("当前版本路径越界")
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("当前版本文件缺失：%w", err)
	}
	if info.IsDir() {
		return "", errors.New("当前版本路径不是可执行文件")
	}
	return target, nil
}

// PreviousVersion finds the highest installed version below current. A
// version counts as installed when its executable exists.
func PreviousVersion(installRoot, current string) (string, error) {
	candidates, err := localVersions(installRoot, current)
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		return "", errors.New("没有可回退的上一版本")
	}
	return candidates[0], nil
}

// localVersions lists complete installed versions below current, newest first.
func localVersions(installRoot, current string) ([]string, error) {
	entries, err := os.ReadDir(installRoot)
	if err != nil {
		return nil, fmt.Errorf("读取安装目录失败：%w", err)
	}
	var candidates []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		v := e.Name()
		if CheckVersion(v) != nil || Compare(v, current) >= 0 {
			continue
		}
		if _, err := os.Stat(filepath.Join(installRoot, v, "PWF Agent.exe")); err != nil {
			continue
		}
		candidates = append(candidates, v)
	}
	sort.Slice(candidates, func(i, j int) bool { return Compare(candidates[i], candidates[j]) > 0 })
	return candidates, nil
}

// PruneVersions deletes installed versions older than the keepPrevious highest
// ones below current. The running version is never touched, so a healthy start
// can call this to bound disk usage without risking a rollback target.
func PruneVersions(installRoot, current string, keepPrevious int) ([]string, error) {
	if keepPrevious < 0 {
		keepPrevious = 0
	}
	candidates, err := localVersions(installRoot, current)
	if err != nil {
		return nil, err
	}
	var removed []string
	for i := keepPrevious; i < len(candidates); i++ {
		if err := os.RemoveAll(filepath.Join(installRoot, candidates[i])); err != nil {
			return removed, fmt.Errorf("清理旧版本 %s 失败：%w", candidates[i], err)
		}
		removed = append(removed, candidates[i])
	}
	return removed, nil
}

// WriteRollbackNotice records a pending rollback for the next boot.
func WriteRollbackNotice(path, from, to string) error {
	return writeJSON(path, RollbackNotice{From: from, To: to, At: time.Now().UTC()})
}

// TakeRollbackNotice reads and removes the notice, if any.
func TakeRollbackNotice(path string) (RollbackNotice, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RollbackNotice{}, false
	}
	_ = os.Remove(path)
	var n RollbackNotice
	if err := json.Unmarshal(data, &n); err != nil {
		return RollbackNotice{}, false
	}
	return n, true
}

func readStartState(path string) StartState {
	data, err := os.ReadFile(path)
	if err != nil {
		return StartState{}
	}
	var st StartState
	if err := json.Unmarshal(data, &st); err != nil {
		return StartState{}
	}
	return st
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".update-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

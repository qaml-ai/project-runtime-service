package workspace

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func (m *Manager) ensureZFSDataset(workspaceName, mountpoint string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("%w: linux/ZFS host required", ErrReflinkCloneUnavailable)
	}
	dataset, err := m.zfsDatasetName(workspaceName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(mountpoint), 0o755); err != nil {
		return err
	}
	if !m.zfsDatasetExists(dataset) {
		args := []string{"create", "-p", "-o", "mountpoint=" + mountpoint}
		if strings.TrimSpace(m.cfg.ZFSRefQuota) != "" && strings.TrimSpace(m.cfg.ZFSRefQuota) != "0" {
			args = append(args, "-o", "refquota="+m.cfg.ZFSRefQuota)
		}
		args = append(args, dataset)
		if err := m.runZFS(args...); err != nil {
			return err
		}
	} else {
		if err := m.runZFS("set", "mountpoint="+mountpoint, dataset); err != nil {
			return err
		}
		if strings.TrimSpace(m.cfg.ZFSRefQuota) != "" && strings.TrimSpace(m.cfg.ZFSRefQuota) != "0" {
			if err := m.runZFS("set", "refquota="+m.cfg.ZFSRefQuota, dataset); err != nil {
				return err
			}
		}
	}
	if mounted, err := m.zfsDatasetMounted(dataset); err != nil {
		return err
	} else if !mounted {
		if err := m.runZFS("mount", dataset); err != nil {
			return err
		}
	}
	if err := ensureWorkspaceOwner(mountpoint); err != nil {
		return err
	}
	return nil
}

func (m *Manager) destroyZFSDataset(workspaceName string) error {
	dataset, err := m.zfsDatasetName(workspaceName)
	if err != nil {
		return err
	}
	if m.zfsDatasetExists(dataset) {
		origin, _ := m.zfsDatasetOrigin(dataset)
		if err := m.runZFS("destroy", "-r", dataset); err != nil {
			return err
		}
		if origin != "" && origin != "-" {
			_ = m.runZFS("destroy", origin)
		}
	}
	workspaceDir := filepath.Join(m.cfg.WorkspacesRoot, workspaceName)
	if err := os.RemoveAll(workspaceDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (m *Manager) CloneZFS(sourceName, targetName string) error {
	if strings.TrimSpace(sourceName) == "" || strings.TrimSpace(targetName) == "" {
		return errors.New("source and target workspace names required")
	}
	if sourceName == targetName {
		return errors.New("source and target workspace names must differ")
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("%w: linux/ZFS host required", ErrReflinkCloneUnavailable)
	}

	firstName, secondName := sourceName, targetName
	if secondName < firstName {
		firstName, secondName = secondName, firstName
	}
	firstMu := m.ensureLock(firstName)
	secondMu := m.ensureLock(secondName)
	firstMu.Lock()
	defer firstMu.Unlock()
	secondMu.Lock()
	defer secondMu.Unlock()

	sourceDataset, err := m.zfsDatasetName(sourceName)
	if err != nil {
		return err
	}
	targetDataset, err := m.zfsDatasetName(targetName)
	if err != nil {
		return err
	}
	if !m.zfsDatasetExists(sourceDataset) {
		return fmt.Errorf("source ZFS dataset does not exist: %s", sourceDataset)
	}
	if m.zfsDatasetExists(targetDataset) {
		return errors.New("target workspace already exists")
	}
	targetDir := filepath.Join(m.cfg.WorkspacesRoot, targetName)
	if _, err := os.Stat(targetDir); err == nil {
		return errors.New("target workspace already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat target workspace: %w", err)
	}

	snapshot := sourceDataset + "@" + zfsSnapshotName("clone")
	if err := m.runZFS("snapshot", snapshot); err != nil {
		return err
	}
	published := false
	targetCreated := false
	defer func() {
		if published {
			return
		}
		if targetCreated {
			_ = m.runZFS("destroy", "-r", targetDataset)
		}
		_ = m.runZFS("destroy", snapshot)
		_ = os.RemoveAll(targetDir)
	}()
	if err := m.runZFS("clone", "-o", "mountpoint="+targetDir, snapshot, targetDataset); err != nil {
		return err
	}
	targetCreated = true
	if strings.TrimSpace(m.cfg.ZFSRefQuota) != "" && strings.TrimSpace(m.cfg.ZFSRefQuota) != "0" {
		if err := m.runZFS("set", "refquota="+m.cfg.ZFSRefQuota, targetDataset); err != nil {
			return err
		}
	}
	if err := ensureWorkspaceOwner(targetDir); err != nil {
		return err
	}

	m.mu.Lock()
	m.mounts[targetName] = m.mountRecord(targetName)
	m.mu.Unlock()
	published = true
	return nil
}

func (m *Manager) WriteZFSBackup(workspaceName, targetPath string) (string, error) {
	dataset, err := m.zfsDatasetName(workspaceName)
	if err != nil {
		return "", err
	}
	if !m.zfsDatasetExists(dataset) {
		return "", fmt.Errorf("ZFS dataset does not exist: %s", dataset)
	}
	snapshotName := zfsSnapshotName("backup")
	snapshot := dataset + "@" + snapshotName
	if err := m.runZFS("snapshot", snapshot); err != nil {
		return "", err
	}
	defer func() {
		_ = m.runZFS("destroy", snapshot)
	}()

	file, err := os.Create(targetPath)
	if err != nil {
		return "", err
	}
	gzipWriter := gzip.NewWriter(file)

	send := exec.Command(m.zfsCommand(), "send", snapshot)
	sendOut, err := send.StdoutPipe()
	if err != nil {
		_ = gzipWriter.Close()
		_ = file.Close()
		return "", err
	}
	sendErr := new(strings.Builder)
	send.Stderr = sendErr
	if err := send.Start(); err != nil {
		_ = gzipWriter.Close()
		_ = file.Close()
		return "", err
	}
	_, copyErr := io.Copy(gzipWriter, sendOut)
	waitErr := send.Wait()
	if copyErr != nil {
		_ = gzipWriter.Close()
		_ = file.Close()
		return "", copyErr
	}
	if waitErr != nil {
		_ = gzipWriter.Close()
		_ = file.Close()
		return "", fmt.Errorf("zfs send %s failed: %w: %s", snapshot, waitErr, strings.TrimSpace(sendErr.String()))
	}
	if err := gzipWriter.Close(); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return snapshotName, nil
}

func (m *Manager) RestoreZFSBackup(workspaceName, sourcePath string) error {
	targetDataset, err := m.zfsDatasetName(workspaceName)
	if err != nil {
		return err
	}
	targetDir := filepath.Join(m.cfg.WorkspacesRoot, workspaceName)
	tempName := workspaceName + "-restore-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	rollbackName := workspaceName + "-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	tempDataset, err := m.zfsDatasetName(tempName)
	if err != nil {
		return err
	}
	rollbackDataset, err := m.zfsDatasetName(rollbackName)
	if err != nil {
		return err
	}

	file, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzipReader.Close()

	recv := exec.Command(m.zfsCommand(), "receive", "-u", tempDataset)
	recv.Stdin = gzipReader
	recvErr := new(strings.Builder)
	recv.Stderr = recvErr
	if err := recv.Run(); err != nil {
		_ = m.runZFS("destroy", "-r", tempDataset)
		return fmt.Errorf("zfs receive %s failed: %w: %s", tempDataset, err, strings.TrimSpace(recvErr.String()))
	}
	defer func() {
		if m.zfsDatasetExists(tempDataset) {
			_ = m.runZFS("destroy", "-r", tempDataset)
		}
	}()

	targetExists := m.zfsDatasetExists(targetDataset)
	if targetExists {
		if err := m.runZFS("rename", targetDataset, rollbackDataset); err != nil {
			return fmt.Errorf("move current project aside: %w", err)
		}
	}
	published := false
	if err := m.runZFS("rename", tempDataset, targetDataset); err != nil {
		if targetExists {
			_ = m.runZFS("rename", rollbackDataset, targetDataset)
		}
		return fmt.Errorf("publish restored project: %w", err)
	}
	published = true
	rollbackPublish := func() {
		if targetExists {
			_ = m.runZFS("rename", targetDataset, tempDataset)
			_ = m.runZFS("rename", rollbackDataset, targetDataset)
			return
		}
		_ = m.runZFS("rename", targetDataset, tempDataset)
	}
	if err := m.runZFS("set", "mountpoint="+targetDir, targetDataset); err != nil {
		rollbackPublish()
		return err
	}
	if strings.TrimSpace(m.cfg.ZFSRefQuota) != "" && strings.TrimSpace(m.cfg.ZFSRefQuota) != "0" {
		if err := m.runZFS("set", "refquota="+m.cfg.ZFSRefQuota, targetDataset); err != nil {
			rollbackPublish()
			return err
		}
	}
	if mounted, err := m.zfsDatasetMounted(targetDataset); err != nil {
		rollbackPublish()
		return err
	} else if !mounted {
		if err := m.runZFS("mount", targetDataset); err != nil {
			rollbackPublish()
			return err
		}
	}
	if err := ensureWorkspaceOwner(targetDir); err != nil {
		rollbackPublish()
		return err
	}
	if targetExists {
		_ = m.runZFS("destroy", "-r", rollbackDataset)
	}
	if published {
		m.mu.Lock()
		m.mounts[workspaceName] = m.mountRecord(workspaceName)
		m.mu.Unlock()
	}
	return nil
}

func (m *Manager) ReplaceWithDirectory(workspaceName, sourceDir string) error {
	if !m.UsesZFS() {
		return errors.New("ReplaceWithDirectory is only supported for ZFS storage")
	}
	if stat, err := os.Stat(sourceDir); err != nil {
		return err
	} else if !stat.IsDir() {
		return errors.New("source is not a directory")
	}

	targetDataset, err := m.zfsDatasetName(workspaceName)
	if err != nil {
		return err
	}
	targetDir := filepath.Join(m.cfg.WorkspacesRoot, workspaceName)
	tempName := workspaceName + "-restore-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	rollbackName := workspaceName + "-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	tempDir := filepath.Join(m.cfg.WorkspacesRoot, "."+tempName)
	tempDataset, err := m.zfsDatasetName(tempName)
	if err != nil {
		return err
	}
	rollbackDataset, err := m.zfsDatasetName(rollbackName)
	if err != nil {
		return err
	}

	if err := m.ensureZFSDataset(tempName, tempDir); err != nil {
		return err
	}
	defer func() {
		m.mu.Lock()
		delete(m.mounts, tempName)
		m.mu.Unlock()
		if m.zfsDatasetExists(tempDataset) {
			_ = m.runZFS("destroy", "-r", tempDataset)
		}
		_ = os.RemoveAll(tempDir)
	}()
	if err := runCommand("cp", "-a", sourceDir+"/.", tempDir+"/"); err != nil {
		return err
	}

	targetExists := m.zfsDatasetExists(targetDataset)
	if targetExists {
		if err := m.runZFS("rename", targetDataset, rollbackDataset); err != nil {
			return fmt.Errorf("move current project aside: %w", err)
		}
	}
	if err := m.runZFS("rename", tempDataset, targetDataset); err != nil {
		if targetExists {
			_ = m.runZFS("rename", rollbackDataset, targetDataset)
		}
		return fmt.Errorf("publish restored project: %w", err)
	}
	rollbackPublish := func() {
		if targetExists {
			_ = m.runZFS("rename", targetDataset, tempDataset)
			_ = m.runZFS("rename", rollbackDataset, targetDataset)
			return
		}
		_ = m.runZFS("rename", targetDataset, tempDataset)
	}
	if err := m.runZFS("set", "mountpoint="+targetDir, targetDataset); err != nil {
		rollbackPublish()
		return err
	}
	if strings.TrimSpace(m.cfg.ZFSRefQuota) != "" && strings.TrimSpace(m.cfg.ZFSRefQuota) != "0" {
		if err := m.runZFS("set", "refquota="+m.cfg.ZFSRefQuota, targetDataset); err != nil {
			rollbackPublish()
			return err
		}
	}
	if mounted, err := m.zfsDatasetMounted(targetDataset); err != nil {
		rollbackPublish()
		return err
	} else if !mounted {
		if err := m.runZFS("mount", targetDataset); err != nil {
			rollbackPublish()
			return err
		}
	}
	if err := ensureWorkspaceOwner(targetDir); err != nil {
		rollbackPublish()
		return err
	}
	if targetExists {
		_ = m.runZFS("destroy", "-r", rollbackDataset)
	}
	m.mu.Lock()
	m.mounts[workspaceName] = m.mountRecord(workspaceName)
	m.mu.Unlock()
	return nil
}

func (m *Manager) zfsDatasetName(workspaceName string) (string, error) {
	pool := strings.Trim(strings.TrimSpace(m.cfg.ZFSPool), "/")
	if pool == "" {
		return "", errors.New("PROJECT_RUNTIME_ZFS_POOL is required when PROJECT_RUNTIME_STORAGE_DRIVER=zfs")
	}
	parts := []string{pool}
	for _, part := range strings.Split(strings.Trim(m.cfg.ZFSDatasetPrefix, "/"), "/") {
		if cleaned := sanitizeZFSComponent(part); cleaned != "" {
			parts = append(parts, cleaned)
		}
	}
	name := sanitizeZFSComponent(workspaceName)
	if name == "" {
		return "", errors.New("workspace name required")
	}
	parts = append(parts, name)
	return strings.Join(parts, "/"), nil
}

func sanitizeZFSComponent(value string) string {
	cleaned := strings.TrimSpace(strings.ToLower(value))
	if cleaned == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range cleaned {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if out == "" {
		return ""
	}
	return out
}

func zfsSnapshotName(prefix string) string {
	return prefix + "-" + time.Now().UTC().Format("20060102T150405.000000000Z")
}

func (m *Manager) zfsDatasetExists(dataset string) bool {
	return m.runZFS("list", "-H", "-o", "name", dataset) == nil
}

func (m *Manager) zfsDatasetMounted(dataset string) (bool, error) {
	output, err := m.runZFSOutput("get", "-H", "-o", "value", "mounted", dataset)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) == "yes", nil
}

func (m *Manager) zfsDatasetOrigin(dataset string) (string, error) {
	output, err := m.runZFSOutput("get", "-H", "-o", "value", "origin", dataset)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

func (m *Manager) runZFS(args ...string) error {
	_, err := m.runZFSOutput(args...)
	return err
}

func (m *Manager) runZFSOutput(args ...string) (string, error) {
	cmd := exec.Command(m.zfsCommand(), args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s failed: %w: %s", m.zfsCommand(), strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func (m *Manager) zfsCommand() string {
	if strings.TrimSpace(m.cfg.ZFSCommand) == "" {
		return "zfs"
	}
	return m.cfg.ZFSCommand
}

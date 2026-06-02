package workspace

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestZFSConfigAndDatasetNames(t *testing.T) {
	manager := NewManager(ManagerConfig{
		WorkspacesRoot:   t.TempDir(),
		StorageDriver:    "zfs",
		ZFSPool:          "tank",
		ZFSDatasetPrefix: "camel/projects",
		ZFSRefQuota:      "25g",
	})

	if !manager.UsesZFS() {
		t.Fatal("expected ZFS storage driver")
	}
	if manager.BackupExtension() != ".zfs.gz" {
		t.Fatalf("unexpected backup extension: %s", manager.BackupExtension())
	}
	if manager.ProjectQuotasEnabled() {
		t.Fatal("XFS project quotas should be disabled in ZFS mode")
	}
	dataset, err := manager.zfsDatasetName("Project Pizza Delivery/../../Main")
	if err != nil {
		t.Fatal(err)
	}
	if dataset != "tank/camel/projects/project-pizza-delivery-main" {
		t.Fatalf("unexpected dataset name: %s", dataset)
	}
}

func TestDirectoryConfigUsesTarBackups(t *testing.T) {
	manager := NewManager(ManagerConfig{WorkspacesRoot: t.TempDir()})

	if manager.UsesZFS() {
		t.Fatal("directory should be the default storage driver")
	}
	if manager.BackupExtension() != ".tar.gz" {
		t.Fatalf("unexpected backup extension: %s", manager.BackupExtension())
	}
}

func TestWriteZFSBackupUsesCompressedSendStream(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "zfs.log")
	fakeZFS := filepath.Join(root, "zfs")
	script := `#!/bin/sh
echo "$@" >> "` + logPath + `"
case "$1" in
  list)
    exit 0
    ;;
  snapshot)
    exit 0
    ;;
  send)
    printf "stream:%s" "$2"
    exit 0
    ;;
  destroy)
    exit 0
    ;;
esac
exit 1
`
	if err := os.WriteFile(fakeZFS, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(ManagerConfig{
		WorkspacesRoot:   filepath.Join(root, "workspaces"),
		StorageDriver:    "zfs",
		ZFSPool:          "tank",
		ZFSDatasetPrefix: "projects",
		ZFSCommand:       fakeZFS,
	})
	targetPath := filepath.Join(root, "backup.zfs.gz")
	snapshotName, err := manager.WriteZFSBackup("project-pizza", targetPath)
	if err != nil {
		t.Fatalf("WriteZFSBackup failed: %v", err)
	}
	if !strings.HasPrefix(snapshotName, "backup-") {
		t.Fatalf("unexpected snapshot name: %s", snapshotName)
	}

	file, err := os.Open(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "snapshot tank/projects/project-pizza@backup-") {
		t.Fatalf("expected snapshot command in log, got:\n%s", string(body))
	}
	uncompressed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(uncompressed), "stream:tank/projects/project-pizza@backup-") {
		t.Fatalf("unexpected send stream: %s", string(uncompressed))
	}
}

func TestDestroyZFSDatasetAttemptsCloneOriginCleanup(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "zfs.log")
	fakeZFS := filepath.Join(root, "zfs")
	script := `#!/bin/sh
echo "$@" >> "` + logPath + `"
case "$1" in
  list)
    exit 0
    ;;
  get)
    if [ "$5" = "origin" ]; then
      printf "tank/projects/source@clone-1\n"
      exit 0
    fi
    exit 1
    ;;
  destroy)
    exit 0
    ;;
esac
exit 1
`
	if err := os.WriteFile(fakeZFS, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(ManagerConfig{
		WorkspacesRoot:   filepath.Join(root, "workspaces"),
		StorageDriver:    "zfs",
		ZFSPool:          "tank",
		ZFSDatasetPrefix: "projects",
		ZFSCommand:       fakeZFS,
	})
	if err := manager.destroyZFSDataset("project-clone"); err != nil {
		t.Fatalf("destroyZFSDataset failed: %v", err)
	}
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(body)
	if !strings.Contains(log, "destroy -r tank/projects/project-clone") {
		t.Fatalf("expected clone dataset destroy in log, got:\n%s", log)
	}
	if !strings.Contains(log, "destroy tank/projects/source@clone-1") {
		t.Fatalf("expected origin snapshot cleanup in log, got:\n%s", log)
	}
}

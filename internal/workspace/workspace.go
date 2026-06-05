package workspace

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrReflinkCloneUnavailable = errors.New("reflink clone unavailable")

type ManagerConfig struct {
	WorkspacesRoot     string
	StorageDriver      string
	EnableProjectQuota bool
	DefaultBlockHard   string
	DefaultInodeHard   string
	ProjectsFile       string
	ProjidFile         string
	ZFSPool            string
	ZFSDatasetPrefix   string
	ZFSRefQuota        string
	ZFSCommand         string
}

type Mount struct {
	Name      string
	MergedDir string
	MountedAt time.Time
}

type Manager struct {
	cfg      ManagerConfig
	mu       sync.RWMutex
	mounts   map[string]*Mount
	ensureMu sync.Map // per-name *sync.Mutex — serializes Ensure() per workspace
	quotaMu  sync.Mutex
}

const defaultProjectIDStart = 10000

func NewManagerFromEnv() *Manager {
	cfg := ManagerConfig{
		WorkspacesRoot:   envString("WORKSPACES_ROOT", defaultWorkspaceRoot()),
		StorageDriver:    envString("PROJECT_RUNTIME_STORAGE_DRIVER", "directory"),
		DefaultBlockHard: envString("PROJECT_RUNTIME_DEFAULT_BHARD", "100g"),
		DefaultInodeHard: envString("PROJECT_RUNTIME_DEFAULT_IHARD", "0"),
		ProjectsFile:     envString("PROJECT_RUNTIME_PROJECTS_FILE", "/etc/projects"),
		ProjidFile:       envString("PROJECT_RUNTIME_PROJID_FILE", "/etc/projid"),
		ZFSPool:          envString("PROJECT_RUNTIME_ZFS_POOL", ""),
		ZFSDatasetPrefix: envString("PROJECT_RUNTIME_ZFS_DATASET_PREFIX", "projects"),
		ZFSRefQuota:      envString("PROJECT_RUNTIME_ZFS_REFQUOTA", envString("PROJECT_RUNTIME_DEFAULT_BHARD", "100g")),
		ZFSCommand:       envString("PROJECT_RUNTIME_ZFS_COMMAND", "zfs"),
	}
	enableQuotaDefault := runtime.GOOS == "linux"
	cfg.EnableProjectQuota = envBool("PROJECT_RUNTIME_ENABLE_PROJECT_QUOTA", enableQuotaDefault)
	return NewManager(cfg)
}

func NewManager(cfg ManagerConfig) *Manager {
	if cfg.WorkspacesRoot == "" {
		cfg.WorkspacesRoot = defaultWorkspaceRoot()
	}
	cfg.StorageDriver = strings.TrimSpace(strings.ToLower(cfg.StorageDriver))
	if cfg.StorageDriver == "" {
		cfg.StorageDriver = "directory"
	}
	if cfg.DefaultBlockHard == "" {
		cfg.DefaultBlockHard = "100g"
	}
	if cfg.DefaultInodeHard == "" {
		cfg.DefaultInodeHard = "0"
	}
	if cfg.ProjectsFile == "" {
		cfg.ProjectsFile = "/etc/projects"
	}
	if cfg.ProjidFile == "" {
		cfg.ProjidFile = "/etc/projid"
	}
	if cfg.ZFSDatasetPrefix == "" {
		cfg.ZFSDatasetPrefix = "projects"
	}
	if cfg.ZFSRefQuota == "" {
		cfg.ZFSRefQuota = cfg.DefaultBlockHard
	}
	if cfg.ZFSCommand == "" {
		cfg.ZFSCommand = "zfs"
	}
	return &Manager{
		cfg:    cfg,
		mounts: make(map[string]*Mount),
	}
}

func (m *Manager) mountRecord(name string) *Mount {
	return &Mount{
		Name:      name,
		MergedDir: filepath.Join(m.cfg.WorkspacesRoot, name),
		MountedAt: time.Now().UTC(),
	}
}

func (m *Manager) ensureLock(name string) *sync.Mutex {
	v, _ := m.ensureMu.LoadOrStore(name, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (m *Manager) Ensure(name string) (string, error) {
	mu := m.ensureLock(name)
	mu.Lock()
	defer mu.Unlock()

	if name == "" {
		return "", errors.New("workspace name required")
	}

	m.mu.RLock()
	existing := m.mounts[name]
	m.mu.RUnlock()
	if existing != nil && m.isWorkspaceReady(existing.MergedDir) {
		if err := ensureWorkspaceOwner(existing.MergedDir); err != nil {
			return "", err
		}
		return existing.MergedDir, nil
	}

	mount := m.mountRecord(name)
	if m.UsesZFS() {
		if err := m.ensureZFSDataset(name, mount.MergedDir); err != nil {
			return "", err
		}
	} else {
		if err := os.MkdirAll(mount.MergedDir, 0o700); err != nil {
			return "", err
		}
		if err := ensureWorkspaceOwner(mount.MergedDir); err != nil {
			return "", err
		}

		if err := m.ensureProjectQuota(name, mount.MergedDir); err != nil {
			return "", err
		}
	}

	m.mu.Lock()
	m.mounts[name] = mount
	m.mu.Unlock()

	return mount.MergedDir, nil
}

func (m *Manager) Root() string {
	return m.cfg.WorkspacesRoot
}

func (m *Manager) CloneReflink(sourceName, targetName string) error {
	if strings.TrimSpace(sourceName) == "" || strings.TrimSpace(targetName) == "" {
		return errors.New("source and target workspace names required")
	}
	if sourceName == targetName {
		return errors.New("source and target workspace names must differ")
	}
	if m.UsesZFS() {
		return m.CloneZFS(sourceName, targetName)
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("%w: linux/XFS host required", ErrReflinkCloneUnavailable)
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

	sourceDir := filepath.Join(m.cfg.WorkspacesRoot, sourceName)
	targetDir := filepath.Join(m.cfg.WorkspacesRoot, targetName)
	targetTmp := filepath.Join(m.cfg.WorkspacesRoot, "."+targetName+".clone-"+strconv.FormatInt(time.Now().UnixNano(), 10))

	info, err := os.Stat(sourceDir)
	if err != nil {
		return fmt.Errorf("stat source workspace: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("source workspace is not a directory")
	}
	if _, err := os.Stat(targetDir); err == nil {
		return fmt.Errorf("target workspace already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat target workspace: %w", err)
	}

	if err := os.MkdirAll(targetTmp, 0o700); err != nil {
		return fmt.Errorf("create target temp workspace: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(targetTmp)
	}()

	if err := runCommand("cp", "-a", "--reflink=always", sourceDir+"/.", targetTmp+"/"); err != nil {
		return fmt.Errorf("%w: %v", ErrReflinkCloneUnavailable, err)
	}
	if err := os.Rename(targetTmp, targetDir); err != nil {
		return fmt.Errorf("publish target clone: %w", err)
	}
	if err := m.ensureProjectQuota(targetName, targetDir); err != nil {
		return err
	}

	m.mu.Lock()
	m.mounts[targetName] = m.mountRecord(targetName)
	m.mu.Unlock()
	return nil
}

func (m *Manager) Delete(name string) error {
	mu := m.ensureLock(name)
	mu.Lock()
	defer mu.Unlock()

	if name == "" {
		return errors.New("workspace name required")
	}

	if m.UsesZFS() {
		if err := m.destroyZFSDataset(name); err != nil {
			return err
		}
	} else {
		workspaceDir := filepath.Join(m.cfg.WorkspacesRoot, name)
		if err := os.RemoveAll(workspaceDir); err != nil {
			return err
		}
	}

	m.mu.Lock()
	delete(m.mounts, name)
	m.mu.Unlock()

	if err := m.deleteProjectQuota(name); err != nil {
		return err
	}

	return nil
}

func (m *Manager) UsesZFS() bool {
	return m.cfg.StorageDriver == "zfs"
}

func (m *Manager) StorageDriver() string {
	return m.cfg.StorageDriver
}

func (m *Manager) ProjectQuotasEnabled() bool {
	return !m.UsesZFS() && m.cfg.EnableProjectQuota && runtime.GOOS == "linux"
}

func (m *Manager) BackupExtension() string {
	if m.UsesZFS() {
		return ".zfs.gz"
	}
	return ".tar.gz"
}

func (m *Manager) isWorkspaceReady(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func ensureWorkspaceOwner(path string) error {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return nil
	}
	if err := filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := os.Lchown(current, 1001, 1001); err != nil {
			return fmt.Errorf("set workspace owner for %s: %w", current, err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("set workspace tree owner: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("set workspace mode: %w", err)
	}
	return nil
}

func (m *Manager) ensureProjectQuota(workspaceName, workspaceDir string) error {
	if !m.cfg.EnableProjectQuota {
		return nil
	}
	if runtime.GOOS != "linux" {
		return nil
	}

	m.quotaMu.Lock()
	defer m.quotaMu.Unlock()

	if err := ensureFileExists(m.cfg.ProjectsFile, 0o644); err != nil {
		return fmt.Errorf("ensure projects file: %w", err)
	}
	if err := ensureFileExists(m.cfg.ProjidFile, 0o644); err != nil {
		return fmt.Errorf("ensure projid file: %w", err)
	}

	projectName := projectNameForWorkspace(workspaceName)
	projectIDs, err := readProjidMap(m.cfg.ProjidFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", m.cfg.ProjidFile, err)
	}
	projectPaths, err := readProjectsMap(m.cfg.ProjectsFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", m.cfg.ProjectsFile, err)
	}

	projectID, ok := projectIDs[projectName]
	if !ok {
		projectID = nextProjectID(projectIDs)
		projectIDs[projectName] = projectID
	}
	projectPaths[projectID] = workspaceDir

	if err := writeProjidMap(m.cfg.ProjidFile, projectIDs); err != nil {
		return fmt.Errorf("write %s: %w", m.cfg.ProjidFile, err)
	}
	if err := writeProjectsMap(m.cfg.ProjectsFile, projectPaths); err != nil {
		return fmt.Errorf("write %s: %w", m.cfg.ProjectsFile, err)
	}

	if err := runXFSQuota(m.cfg.WorkspacesRoot, fmt.Sprintf("project -s %s", projectName)); err != nil {
		return fmt.Errorf("project init failed for %s: %w", projectName, err)
	}
	if err := runXFSQuota(
		m.cfg.WorkspacesRoot,
		fmt.Sprintf("limit -p bhard=%s ihard=%s %s", m.cfg.DefaultBlockHard, m.cfg.DefaultInodeHard, projectName),
	); err != nil {
		return fmt.Errorf("project limit failed for %s: %w", projectName, err)
	}

	return nil
}

func (m *Manager) deleteProjectQuota(workspaceName string) error {
	if !m.cfg.EnableProjectQuota {
		return nil
	}
	if runtime.GOOS != "linux" {
		return nil
	}

	m.quotaMu.Lock()
	defer m.quotaMu.Unlock()

	projectIDs, err := readProjidMap(m.cfg.ProjidFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", m.cfg.ProjidFile, err)
	}
	projectPaths, err := readProjectsMap(m.cfg.ProjectsFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", m.cfg.ProjectsFile, err)
	}

	projectName := projectNameForWorkspace(workspaceName)
	projectID, ok := projectIDs[projectName]
	if !ok {
		return nil
	}

	delete(projectIDs, projectName)
	delete(projectPaths, projectID)

	if err := writeProjidMap(m.cfg.ProjidFile, projectIDs); err != nil {
		return fmt.Errorf("write %s: %w", m.cfg.ProjidFile, err)
	}
	if err := writeProjectsMap(m.cfg.ProjectsFile, projectPaths); err != nil {
		return fmt.Errorf("write %s: %w", m.cfg.ProjectsFile, err)
	}

	return nil
}

func ensureFileExists(path string, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE, perm)
	if err != nil {
		return err
	}
	return f.Close()
}

func projectNameForWorkspace(workspace string) string {
	trimmed := strings.TrimSpace(strings.ToLower(workspace))
	if trimmed == "" {
		return "sb_unknown"
	}
	var b strings.Builder
	b.Grow(len(trimmed) + 3)
	b.WriteString("sb_")
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

func nextProjectID(existing map[string]int) int {
	maxID := defaultProjectIDStart - 1
	for _, id := range existing {
		if id > maxID {
			maxID = id
		}
	}
	return maxID + 1
}

func readProjidMap(path string) (map[string]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make(map[string]int)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		idRaw := strings.TrimSpace(parts[1])
		if name == "" || idRaw == "" {
			continue
		}
		id, parseErr := strconv.Atoi(idRaw)
		if parseErr != nil {
			continue
		}
		out[name] = id
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func readProjectsMap(path string) (map[int]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make(map[int]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		idRaw := strings.TrimSpace(parts[0])
		pathValue := strings.TrimSpace(parts[1])
		if idRaw == "" || pathValue == "" {
			continue
		}
		id, parseErr := strconv.Atoi(idRaw)
		if parseErr != nil {
			continue
		}
		out[id] = pathValue
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func writeProjidMap(path string, values map[string]int) error {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	lines := make([]string, 0, len(names))
	for _, name := range names {
		lines = append(lines, fmt.Sprintf("%s:%d", name, values[name]))
	}
	return writeLinesAtomic(path, lines)
}

func writeProjectsMap(path string, values map[int]string) error {
	ids := make([]int, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	lines := make([]string, 0, len(ids))
	for _, id := range ids {
		lines = append(lines, fmt.Sprintf("%d:%s", id, values[id]))
	}
	return writeLinesAtomic(path, lines)
}

func writeLinesAtomic(path string, lines []string) error {
	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, ".tmp-quota-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()

	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}()

	for _, line := range lines {
		if _, err := tmpFile.WriteString(line + "\n"); err != nil {
			return err
		}
	}
	if err := tmpFile.Sync(); err != nil {
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func runXFSQuota(mountRoot, command string) error {
	cmd := exec.Command("xfs_quota", "-x", "-c", command, mountRoot)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("xfs_quota -x -c %q %s failed: %w: %s", command, mountRoot, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s failed: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func envString(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}

func defaultLocalRoot() string {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return ".project-runtime"
	}
	return filepath.Join(wd, ".project-runtime")
}

func defaultWorkspaceRoot() string {
	if runtime.GOOS == "linux" {
		return "/srv/project-runtime"
	}
	return filepath.Join(defaultLocalRoot(), "workspaces")
}

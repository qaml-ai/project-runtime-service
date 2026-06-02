package container

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type containerR2Config struct {
	credentialsDir         string
	uploadsCredentialsFile string
	outputsCredentialsFile string
	uploadsPrefix          string
	outputsPrefix          string
}

type r2TempCredentialOptions struct {
	scope      string
	prefix     string
	ttlSeconds int
	now        time.Time
}

func (m *Manager) prepareContainerR2Config(containerName, orgID, workspaceID string) (*containerR2Config, error) {
	if !m.r2MountsEnabled() {
		return nil, nil
	}
	if err := validateR2PathSegment(orgID, "org id"); err != nil {
		return nil, err
	}
	if err := validateR2PathSegment(workspaceID, "workspace id"); err != nil {
		return nil, err
	}

	workspacePrefix := buildR2WorkspacePrefix(orgID, workspaceID)
	cfg := &containerR2Config{
		credentialsDir:         m.containerR2CredentialsDir(containerName),
		uploadsCredentialsFile: filepath.Join(m.containerR2CredentialsDir(containerName), "uploads.credentials"),
		outputsCredentialsFile: filepath.Join(m.containerR2CredentialsDir(containerName), "outputs.credentials"),
		uploadsPrefix:          workspacePrefix + "user-uploads/",
		outputsPrefix:          workspacePrefix + "user-outputs/",
	}

	if err := os.RemoveAll(cfg.credentialsDir); err != nil {
		return nil, fmt.Errorf("remove stale R2 credential dir: %w", err)
	}
	if err := os.MkdirAll(cfg.credentialsDir, 0700); err != nil {
		return nil, fmt.Errorf("create R2 credential dir: %w", err)
	}

	now := time.Now()
	uploadsCred, err := m.createR2TempCredential(r2TempCredentialOptions{
		scope:      "object-read-only",
		prefix:     cfg.uploadsPrefix,
		ttlSeconds: m.r2TempCredentialTTLSeconds,
		now:        now,
	})
	if err != nil {
		_ = os.RemoveAll(cfg.credentialsDir)
		return nil, fmt.Errorf("create R2 uploads credential: %w", err)
	}
	outputsCred, err := m.createR2TempCredential(r2TempCredentialOptions{
		scope:      "object-read-write",
		prefix:     cfg.outputsPrefix,
		ttlSeconds: m.r2TempCredentialTTLSeconds,
		now:        now,
	})
	if err != nil {
		_ = os.RemoveAll(cfg.credentialsDir)
		return nil, fmt.Errorf("create R2 outputs credential: %w", err)
	}

	if err := writeAWSCredentialsFile(cfg.uploadsCredentialsFile, uploadsCred); err != nil {
		_ = os.RemoveAll(cfg.credentialsDir)
		return nil, err
	}
	if err := writeAWSCredentialsFile(cfg.outputsCredentialsFile, outputsCred); err != nil {
		_ = os.RemoveAll(cfg.credentialsDir)
		return nil, err
	}
	return cfg, nil
}

func (m *Manager) cleanupContainerR2Config(containerName string) error {
	if strings.TrimSpace(m.r2CredentialsRoot) == "" {
		return nil
	}
	if err := os.RemoveAll(m.containerR2CredentialsDir(containerName)); err != nil {
		return fmt.Errorf("remove R2 credential dir: %w", err)
	}
	return nil
}

func (m *Manager) ResolveR2HostPath(containerName, sandboxPath string) (string, bool) {
	return "", false
}

func (m *Manager) r2MountsEnabled() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if strings.TrimSpace(m.r2CredentialsRoot) == "" ||
		strings.TrimSpace(m.r2BucketName) == "" ||
		strings.TrimSpace(m.r2AccountID) == "" {
		return false
	}
	return m.hasR2Credentials()
}

func (m *Manager) createR2TempCredential(opts r2TempCredentialOptions) (awsCredential, error) {
	if !m.hasR2Credentials() {
		return awsCredential{}, fmt.Errorf("R2 credentials are not configured")
	}
	ttlSeconds := opts.ttlSeconds
	if ttlSeconds <= 0 {
		ttlSeconds = defaultR2TempCredentialTTLSeconds()
	}
	now := opts.now
	if now.IsZero() {
		now = time.Now()
	}

	claims := map[string]any{
		"bucket": m.r2BucketName,
		"scope":  opts.scope,
		"paths": map[string]any{
			"prefixPaths": []string{opts.prefix},
			"objectPaths": []string{},
		},
		"sub": m.r2AccountID,
		"iss": m.r2AccessKeyID,
		"aud": m.r2EndpointHost(),
		"iat": now.Unix(),
		"exp": now.Add(time.Duration(ttlSeconds) * time.Second).Unix(),
	}

	token, err := signR2TempCredentialJWT(claims, m.r2SecretAccessKey)
	if err != nil {
		return awsCredential{}, err
	}
	secretDigest := sha256.Sum256([]byte(token))
	return awsCredential{
		AccessKeyID:     m.r2AccessKeyID,
		SecretAccessKey: hex.EncodeToString(secretDigest[:]),
		SessionToken:    base64.StdEncoding.EncodeToString([]byte("jwt/" + token)),
	}, nil
}

func (m *Manager) hasR2Credentials() bool {
	return strings.TrimSpace(m.r2AccessKeyID) != "" && strings.TrimSpace(m.r2SecretAccessKey) != ""
}

func (m *Manager) containerR2CredentialsDir(containerName string) string {
	return filepath.Join(m.r2CredentialsRoot, safeR2MountName(containerName))
}

func (m *Manager) r2EndpointHost() string {
	return strings.TrimSpace(m.r2AccountID) + ".r2.cloudflarestorage.com"
}

func buildR2WorkspacePrefix(orgID, workspaceID string) string {
	return orgID + "/" + workspaceID + "/"
}

type awsCredential struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

func writeAWSCredentialsFile(path string, cred awsCredential) error {
	contents := fmt.Sprintf("[default]\naws_access_key_id = %s\naws_secret_access_key = %s\naws_session_token = %s\n", cred.AccessKeyID, cred.SecretAccessKey, cred.SessionToken)
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		return fmt.Errorf("write R2 credential file: %w", err)
	}
	return nil
}

func signR2TempCredentialJWT(claims map[string]any, secret string) (string, error) {
	headerBytes, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claimsBytes, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	unsigned := base64.RawURLEncoding.EncodeToString(headerBytes) + "." + base64.RawURLEncoding.EncodeToString(claimsBytes)
	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write([]byte(unsigned)); err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func validateR2PathSegment(value, label string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("%s is empty", label)
	}
	if trimmed != value || strings.Contains(trimmed, "/") || strings.Contains(trimmed, string(filepath.Separator)) {
		return fmt.Errorf("%s contains invalid path characters", label)
	}
	if trimmed == "." || trimmed == ".." || filepath.Clean(trimmed) != trimmed {
		return fmt.Errorf("%s is not a safe path segment", label)
	}
	return nil
}

func safeR2MountName(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range trimmed {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func defaultR2CredentialsRoot() string {
	if runtime.GOOS == "linux" {
		return "/run/project-runtime-r2-creds"
	}
	return ""
}

func defaultR2TempCredentialTTLSeconds() int {
	return 24 * 60 * 60
}

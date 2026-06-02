package container

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCreateR2TempCredentialScopesToPrefix(t *testing.T) {
	m := &Manager{
		r2BucketName:      "chiridion-sandbox",
		r2AccountID:       "account123",
		r2AccessKeyID:     "parent-key",
		r2SecretAccessKey: "parent-secret",
	}

	cred, err := m.createR2TempCredential(r2TempCredentialOptions{
		scope:      "object-read-write",
		prefix:     "org-1/workspace-1/user-outputs/",
		ttlSeconds: 900,
		now:        time.Unix(1_700_000_000, 0),
	})
	if err != nil {
		t.Fatalf("createR2TempCredential returned error: %v", err)
	}
	if cred.AccessKeyID != "parent-key" {
		t.Fatalf("unexpected access key id: %q", cred.AccessKeyID)
	}
	if cred.SecretAccessKey == "" || cred.SecretAccessKey == "parent-secret" {
		t.Fatalf("temporary secret was not derived")
	}

	tokenBytes, err := base64.StdEncoding.DecodeString(cred.SessionToken)
	if err != nil {
		t.Fatalf("decode session token: %v", err)
	}
	token := strings.TrimPrefix(string(tokenBytes), "jwt/")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("unexpected jwt shape: %q", token)
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode jwt payload: %v", err)
	}
	var claims struct {
		Bucket string `json:"bucket"`
		Scope  string `json:"scope"`
		Sub    string `json:"sub"`
		Iss    string `json:"iss"`
		Aud    string `json:"aud"`
		Iat    int64  `json:"iat"`
		Exp    int64  `json:"exp"`
		Paths  struct {
			PrefixPaths []string `json:"prefixPaths"`
			ObjectPaths []string `json:"objectPaths"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		t.Fatalf("unmarshal jwt payload: %v", err)
	}

	if claims.Bucket != "chiridion-sandbox" ||
		claims.Scope != "object-read-write" ||
		claims.Sub != "account123" ||
		claims.Iss != "parent-key" ||
		claims.Aud != "account123.r2.cloudflarestorage.com" ||
		claims.Iat != 1_700_000_000 ||
		claims.Exp != 1_700_000_900 {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	if len(claims.Paths.PrefixPaths) != 1 || claims.Paths.PrefixPaths[0] != "org-1/workspace-1/user-outputs/" {
		t.Fatalf("unexpected prefix paths: %#v", claims.Paths.PrefixPaths)
	}
	if len(claims.Paths.ObjectPaths) != 0 {
		t.Fatalf("unexpected object paths: %#v", claims.Paths.ObjectPaths)
	}
}

func TestPrepareContainerR2ConfigWritesSeparateUploadAndOutputCredentials(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("R2 container FUSE config is only enabled on linux")
	}

	root := t.TempDir()
	m := &Manager{
		r2CredentialsRoot:          root,
		r2BucketName:               "chiridion-sandbox",
		r2AccountID:                "account123",
		r2AccessKeyID:              "parent-key",
		r2SecretAccessKey:          "parent-secret",
		r2TempCredentialTTLSeconds: 900,
	}

	cfg, err := m.prepareContainerR2Config("chiridion/ws:abc", "org-1", "workspace-1")
	if err != nil {
		t.Fatalf("prepareContainerR2Config returned error: %v", err)
	}
	if cfg.uploadsPrefix != "org-1/workspace-1/user-uploads/" {
		t.Fatalf("unexpected uploads prefix: %q", cfg.uploadsPrefix)
	}
	if cfg.outputsPrefix != "org-1/workspace-1/user-outputs/" {
		t.Fatalf("unexpected outputs prefix: %q", cfg.outputsPrefix)
	}
	if cfg.credentialsDir != filepath.Join(root, "chiridion_ws_abc") {
		t.Fatalf("unexpected credential dir: %q", cfg.credentialsDir)
	}

	for _, path := range []string{cfg.uploadsCredentialsFile, cfg.outputsCredentialsFile} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat credential file %s: %v", path, err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("credential file %s mode = %v, want 0600", path, info.Mode().Perm())
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(contents)
		if !strings.Contains(text, "aws_access_key_id = parent-key") ||
			!strings.Contains(text, "aws_secret_access_key = ") ||
			!strings.Contains(text, "aws_session_token = ") {
			t.Fatalf("credential file %s missing expected fields: %q", path, text)
		}
		if strings.Contains(text, "parent-secret") {
			t.Fatalf("credential file %s leaked parent secret", path)
		}
	}
}

func TestValidateR2PathSegmentRejectsTraversal(t *testing.T) {
	for _, value := range []string{"", ".", "..", "../workspace", "org/workspace", " org"} {
		if err := validateR2PathSegment(value, "test"); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}

	for _, value := range []string{"org-1", "workspace_2", "550e8400-e29b-41d4-a716-446655440000"} {
		if err := validateR2PathSegment(value, "test"); err != nil {
			t.Fatalf("expected %q to be accepted: %v", value, err)
		}
	}
}

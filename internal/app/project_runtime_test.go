package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProjectNameNormalization(t *testing.T) {
	got := projectName("Pizza Delivery/../../Main")
	want := "project-pizza-delivery-main"
	if got != want {
		t.Fatalf("projectName mismatch: got %q want %q", got, want)
	}
}

func TestParseProjectRoute(t *testing.T) {
	route, ok := parseProjectRoute("/v1/projects/pizza-delivery/fs/read")
	if !ok {
		t.Fatal("expected route to parse")
	}
	if route.ID != "pizza-delivery" || route.Name != "project-pizza-delivery" || route.Subpath != "/fs/read" {
		t.Fatalf("unexpected route: %+v", route)
	}
}

func TestBearerControlAuth(t *testing.T) {
	server := &Server{cfg: Config{ControlAuthType: "bearer", ControlBearerToken: "secret"}}
	req := httptest.NewRequest(http.MethodGet, "/v1/host/capabilities", nil)
	rec := httptest.NewRecorder()

	if !server.rejectUnauthenticatedControlRequest(rec, req) {
		t.Fatal("expected missing bearer token to be rejected")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/host/capabilities", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	if server.rejectUnauthenticatedControlRequest(rec, req) {
		t.Fatal("expected matching bearer token to pass")
	}
}

func TestProxyCapabilityParsingAndHeaders(t *testing.T) {
	cfg := Config{ProxyCapabilitiesJSON: `{
		"capabilities": [
			{"name":"cf-api","target":"https://api.example.com","bearerToken":"tok","allowedProjects":["pizza-delivery"]}
		]
	}`}
	capabilities := loadProxyCapabilities(cfg)
	capability, ok := capabilities["cf-api"]
	if !ok {
		t.Fatal("expected cf-api capability")
	}
	if !capabilityAllowsProject(capability, "project-pizza-delivery", "pizza-delivery") {
		t.Fatal("expected project id attachment to match")
	}
	if capabilityAllowsProject(capability, "project-other", "other") {
		t.Fatal("did not expect unrelated project to match")
	}

	source := http.Header{}
	source.Set("X-Project-Runtime-Project", "spoofed")
	source.Set("X-Chiridion-Workspace-Id", "spoofed")
	source.Set("X-Keep", "yes")
	target := http.Header{}
	copyProxyHeaders(target, source)
	if target.Get("X-Project-Runtime-Project") != "" || target.Get("X-Chiridion-Workspace-Id") != "" {
		t.Fatalf("expected spoofable headers to be stripped: %+v", target)
	}
	if target.Get("X-Keep") != "yes" {
		t.Fatalf("expected normal header to be kept")
	}
}

func TestGenericProxyPathAndTarget(t *testing.T) {
	name, suffix, ok := parseGenericProxyPath("/p/github/repos/qaml-ai/project-runtime-service")
	if !ok || name != "github" || suffix != "/repos/qaml-ai/project-runtime-service" {
		t.Fatalf("unexpected proxy path parse: name=%q suffix=%q ok=%v", name, suffix, ok)
	}
	target, err := buildProxyTargetURL("https://api.github.com", suffix, "per_page=1")
	if err != nil {
		t.Fatalf("buildProxyTargetURL failed: %v", err)
	}
	if target != "https://api.github.com/repos/qaml-ai/project-runtime-service?per_page=1" {
		t.Fatalf("unexpected target URL: %s", target)
	}
}

func TestSafeArchiveTargetRejectsTraversal(t *testing.T) {
	for _, name := range []string{"../secret", "/absolute", "nested/../../secret"} {
		if _, err := safeArchiveTarget("/tmp/project", name); err == nil {
			t.Fatalf("expected traversal path %q to be rejected", name)
		}
	}
	target, err := safeArchiveTarget("/tmp/project", "src/main.go")
	if err != nil {
		t.Fatalf("safeArchiveTarget failed: %v", err)
	}
	if !strings.HasSuffix(target, "/tmp/project/src/main.go") {
		t.Fatalf("unexpected target: %s", target)
	}
}

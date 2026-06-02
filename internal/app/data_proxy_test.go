package app

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func defaultDataProxyCfg() DataProxyHandlerConfig {
	return DefaultDataProxyHandlerConfig()
}

func TestHandleDataProxyMssqlQueryRejectsInvalidJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/data-proxy/mssql/query", strings.NewReader("{"))
	rec := httptest.NewRecorder()

	if err := handleDataProxyMssqlQueryWithConfig(rec, req, defaultDataProxyCfg()); err != nil {
		t.Fatalf("handleDataProxyMssqlQueryWithConfig returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: got=%d want=%d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleDataProxyMssqlQueryRejectsMissingFields(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/data-proxy/mssql/query", strings.NewReader(`{"server":"db.example.com"}`))
	rec := httptest.NewRecorder()

	if err := handleDataProxyMssqlQueryWithConfig(rec, req, defaultDataProxyCfg()); err != nil {
		t.Fatalf("handleDataProxyMssqlQueryWithConfig returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: got=%d want=%d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleDataProxyMssqlQueryRejectsMissingMode(t *testing.T) {
	body := `{"server":"db.example.com","user":"u","password":"p","query":"select 1"}`
	req := httptest.NewRequest(http.MethodPost, "/data-proxy/mssql/query", strings.NewReader(body))
	rec := httptest.NewRecorder()

	if err := handleDataProxyMssqlQueryWithConfig(rec, req, defaultDataProxyCfg()); err != nil {
		t.Fatalf("handleDataProxyMssqlQueryWithConfig returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: got=%d want=%d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleDataProxyPostgresQueryRejectsInvalidJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/data-proxy/postgres/query", strings.NewReader("{"))
	rec := httptest.NewRecorder()

	if err := handleDataProxyPostgresQueryWithConfig(rec, req, defaultDataProxyCfg()); err != nil {
		t.Fatalf("handleDataProxyPostgresQueryWithConfig returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: got=%d want=%d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleDataProxyPostgresQueryRejectsMissingFields(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/data-proxy/postgres/query", strings.NewReader(`{"host":"db.example.com"}`))
	rec := httptest.NewRecorder()

	if err := handleDataProxyPostgresQueryWithConfig(rec, req, defaultDataProxyCfg()); err != nil {
		t.Fatalf("handleDataProxyPostgresQueryWithConfig returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: got=%d want=%d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleDataProxyPostgresQueryRejectsServerAliasWithoutHost(t *testing.T) {
	body := `{"mode":"read","server":"db.example.com","user":"u","password":"p","query":"select 1","params":[]}`
	req := httptest.NewRequest(http.MethodPost, "/data-proxy/postgres/query", strings.NewReader(body))
	rec := httptest.NewRecorder()

	if err := handleDataProxyPostgresQueryWithConfig(rec, req, defaultDataProxyCfg()); err != nil {
		t.Fatalf("handleDataProxyPostgresQueryWithConfig returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: got=%d want=%d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleDataProxyPostgresQueryRejectsNonArrayParams(t *testing.T) {
	body := `{"mode":"read","host":"db.example.com","user":"u","password":"p","query":"select 1","params":{"id":1}}`
	req := httptest.NewRequest(http.MethodPost, "/data-proxy/postgres/query", strings.NewReader(body))
	rec := httptest.NewRecorder()

	if err := handleDataProxyPostgresQueryWithConfig(rec, req, defaultDataProxyCfg()); err != nil {
		t.Fatalf("handleDataProxyPostgresQueryWithConfig returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: got=%d want=%d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleDataProxyPostgresQueryRejectsInvalidMode(t *testing.T) {
	body := `{"mode":"auto","host":"db.example.com","user":"u","password":"p","query":"select 1","params":[]}`
	req := httptest.NewRequest(http.MethodPost, "/data-proxy/postgres/query", strings.NewReader(body))
	rec := httptest.NewRecorder()

	if err := handleDataProxyPostgresQueryWithConfig(rec, req, defaultDataProxyCfg()); err != nil {
		t.Fatalf("handleDataProxyPostgresQueryWithConfig returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: got=%d want=%d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleDataProxyMySQLQueryRejectsInvalidJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/data-proxy/mysql/query", strings.NewReader("{"))
	rec := httptest.NewRecorder()

	if err := handleDataProxyMySQLQueryWithConfig(rec, req, defaultDataProxyCfg()); err != nil {
		t.Fatalf("handleDataProxyMySQLQueryWithConfig returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: got=%d want=%d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleDataProxyMySQLQueryRejectsMissingFields(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/data-proxy/mysql/query", strings.NewReader(`{"host":"db.example.com"}`))
	rec := httptest.NewRecorder()

	if err := handleDataProxyMySQLQueryWithConfig(rec, req, defaultDataProxyCfg()); err != nil {
		t.Fatalf("handleDataProxyMySQLQueryWithConfig returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: got=%d want=%d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleDataProxyMySQLQueryRejectsServerAliasWithoutHost(t *testing.T) {
	body := `{"mode":"read","server":"db.example.com","user":"u","password":"p","query":"select 1","params":[]}`
	req := httptest.NewRequest(http.MethodPost, "/data-proxy/mysql/query", strings.NewReader(body))
	rec := httptest.NewRecorder()

	if err := handleDataProxyMySQLQueryWithConfig(rec, req, defaultDataProxyCfg()); err != nil {
		t.Fatalf("handleDataProxyMySQLQueryWithConfig returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: got=%d want=%d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleDataProxyMySQLQueryRejectsNonArrayParams(t *testing.T) {
	body := `{"mode":"read","host":"db.example.com","user":"u","password":"p","query":"select 1","params":{"id":1}}`
	req := httptest.NewRequest(http.MethodPost, "/data-proxy/mysql/query", strings.NewReader(body))
	rec := httptest.NewRecorder()

	if err := handleDataProxyMySQLQueryWithConfig(rec, req, defaultDataProxyCfg()); err != nil {
		t.Fatalf("handleDataProxyMySQLQueryWithConfig returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: got=%d want=%d", rec.Code, http.StatusBadRequest)
	}
}

func TestPositionalArgsConvertsIntegralJSONNumbers(t *testing.T) {
	args, err := positionalArgs([]any{"camel", float64(100), float64(1.25)})
	if err != nil {
		t.Fatalf("positionalArgs returned error: %v", err)
	}
	want := []any{"camel", int64(100), float64(1.25)}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("unexpected args: got=%#v want=%#v", args, want)
	}
}

func TestSQLConnectionStringsCanTargetTunnelEndpoint(t *testing.T) {
	pg := postgresQueryRequest{
		Host:     "db.example.com",
		User:     "u",
		Password: "p",
		Database: "app",
		SSLMode:  "require",
	}
	if got := pg.connectionStringFor("127.0.0.1", 15432); !strings.Contains(got, "127.0.0.1:15432") {
		t.Fatalf("postgres connection string did not use tunnel endpoint: %s", got)
	}

	mysql := mysqlQueryRequest{
		Host:     "mysql.example.com",
		User:     "u",
		Password: "p",
		Database: "app",
	}
	got, err := mysql.connectionStringFor("127.0.0.1", 13306)
	if err != nil {
		t.Fatalf("mysql connectionStringFor returned error: %v", err)
	}
	if !strings.Contains(got, "tcp(127.0.0.1:13306)") {
		t.Fatalf("mysql connection string did not use tunnel endpoint: %s", got)
	}

	mssql := mssqlQueryRequest{
		Server:   "sql.example.com",
		User:     "u",
		Password: "p",
		Database: "app",
	}
	if got := mssql.connectionStringFor("127.0.0.1", 11433); !strings.Contains(got, "127.0.0.1:11433") {
		t.Fatalf("mssql connection string did not use tunnel endpoint: %s", got)
	}
}

func TestExpandSSHProxyCommand(t *testing.T) {
	got := expandSSHProxyCommand(
		"cloudflared access ssh --hostname %h --port %p --user %r --literal %%",
		"egress.camelai.dev",
		443,
		"tunnel",
	)
	want := "cloudflared access ssh --hostname egress.camelai.dev --port 443 --user tunnel --literal %"
	if got != want {
		t.Fatalf("unexpected proxy command: got=%q want=%q", got, want)
	}
}

func TestBeginReadTransactionUsesReadOnlyWhenSupported(t *testing.T) {
	calls := 0
	tx, err := beginReadTransaction(func(opts *sql.TxOptions) (*sql.Tx, error) {
		calls++
		if opts == nil || !opts.ReadOnly {
			t.Fatalf("expected first call with read-only transaction options")
		}
		return &sql.Tx{}, nil
	})
	if err != nil {
		t.Fatalf("beginReadTransaction returned error: %v", err)
	}
	if tx == nil {
		t.Fatalf("expected transaction, got nil")
	}
	if calls != 1 {
		t.Fatalf("unexpected begin attempts: got=%d want=1", calls)
	}
}

func TestBeginReadTransactionFallsBackWhenReadOnlyUnsupported(t *testing.T) {
	calls := 0
	tx, err := beginReadTransaction(func(opts *sql.TxOptions) (*sql.Tx, error) {
		calls++
		if calls == 1 {
			if opts == nil || !opts.ReadOnly {
				t.Fatalf("expected first call with read-only transaction options")
			}
			return nil, errors.New("read transactions are not supported")
		}
		if opts != nil {
			t.Fatalf("expected fallback call without transaction options")
		}
		return &sql.Tx{}, nil
	})
	if err != nil {
		t.Fatalf("beginReadTransaction returned error: %v", err)
	}
	if tx == nil {
		t.Fatalf("expected fallback transaction, got nil")
	}
	if calls != 2 {
		t.Fatalf("unexpected begin attempts: got=%d want=2", calls)
	}
}

func TestBeginReadTransactionReturnsOriginalErrorWhenNotReadOnlyUnsupported(t *testing.T) {
	expectedErr := errors.New("network broke")
	calls := 0
	tx, err := beginReadTransaction(func(opts *sql.TxOptions) (*sql.Tx, error) {
		calls++
		return nil, expectedErr
	})
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected original error, got: %v", err)
	}
	if tx != nil {
		t.Fatalf("expected nil transaction on error")
	}
	if calls != 1 {
		t.Fatalf("unexpected begin attempts: got=%d want=1", calls)
	}
}

func TestIsReadOnlyTransactionUnsupportedError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "mssql phrasing", err: errors.New("read transactions are not supported"), want: true},
		{name: "hyphenated", err: errors.New("Read-Only transactions are not supported"), want: true},
		{name: "spaced", err: errors.New("read only transactions are not supported"), want: true},
		{name: "different error", err: errors.New("permission denied"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isReadOnlyTransactionUnsupportedError(tc.err)
			if got != tc.want {
				t.Fatalf("unexpected result: got=%v want=%v", got, tc.want)
			}
		})
	}
}

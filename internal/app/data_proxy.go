package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	neturl "net/url"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	pq "github.com/lib/pq"
	mssql "github.com/microsoft/go-mssqldb"
)

const (
	defaultMssqlPort                        = 1433
	defaultPostgresPort                     = 5432
	defaultMySQLPort                        = 3306
	defaultMssqlDatabase                    = "master"
	defaultPostgresDatabase                 = "postgres"
	defaultMssqlConnectTimeout              = 15 * time.Second
	defaultPostgresConnectMS                = 15 * time.Second
	defaultMySQLConnectMS                   = 15 * time.Second
	defaultMssqlQueryTimeout                = 30 * time.Second
	defaultPostgresQueryMS                  = 30 * time.Second
	defaultMySQLQueryMS                     = 30 * time.Second
	defaultPostgresSSLMode                  = "require"
	defaultMySQLTLS                         = "preferred"
	defaultMySQLCharset                     = "utf8mb4"
	defaultDataProxyRequestLimitBytes int64 = 1 << 20
)

type DataProxyHandlerConfig struct {
	RequestBodyLimitBytes int64
	TunnelManager         *SSHTunnelManager
}

func DefaultDataProxyHandlerConfig() DataProxyHandlerConfig {
	return DataProxyHandlerConfig{
		RequestBodyLimitBytes: defaultDataProxyRequestLimitBytes,
	}
}

func normalizeDataProxyHandlerConfig(cfg DataProxyHandlerConfig) DataProxyHandlerConfig {
	defaults := DefaultDataProxyHandlerConfig()
	if cfg.RequestBodyLimitBytes <= 0 {
		cfg.RequestBodyLimitBytes = defaults.RequestBodyLimitBytes
	}
	return cfg
}

type dataProxyHTTPHandler struct {
	cfg DataProxyHandlerConfig
}

func NewDataProxyHandler(cfg DataProxyHandlerConfig) http.Handler {
	return &dataProxyHTTPHandler{cfg: normalizeDataProxyHandlerConfig(cfg)}
}

func (h *dataProxyHTTPHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	normalizedPath, ok := normalizeDataProxyPath(req.URL.Path)
	if !ok {
		errorJSON(w, "Not found", http.StatusNotFound)
		return
	}

	switch {
	case normalizedPath == "/data-proxy/mssql/query" && req.Method == http.MethodPost:
		_ = handleDataProxyMssqlQueryWithConfig(w, req, h.cfg)
	case normalizedPath == "/data-proxy/postgres/query" && req.Method == http.MethodPost:
		_ = handleDataProxyPostgresQueryWithConfig(w, req, h.cfg)
	case normalizedPath == "/data-proxy/mysql/query" && req.Method == http.MethodPost:
		_ = handleDataProxyMySQLQueryWithConfig(w, req, h.cfg)
	default:
		errorJSON(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func normalizeDataProxyPath(path string) (string, bool) {
	trimmed := strings.TrimSpace(path)

	switch trimmed {
	case "/data-proxy/mssql/query", "/data-proxy/postgres/query", "/data-proxy/mysql/query":
		return trimmed, true
	default:
		return "", false
	}
}

type sqlQueryResponse struct {
	Recordset    []map[string]any `json:"recordset,omitempty"`
	RowsAffected []int64          `json:"rowsAffected,omitempty"`
}

type sqlQueryMode string

const (
	sqlQueryModeRead   sqlQueryMode = "read"
	sqlQueryModeModify sqlQueryMode = "modify"
)

type mssqlQueryRequest struct {
	Mode                   string         `json:"mode"`
	Server                 string         `json:"server"`
	Port                   int            `json:"port"`
	User                   string         `json:"user"`
	Password               string         `json:"password"`
	Database               string         `json:"database"`
	Query                  string         `json:"query"`
	Params                 map[string]any `json:"params"`
	Encrypt                *bool          `json:"encrypt"`
	TrustServerCertificate *bool          `json:"trustServerCertificate"`
}

type postgresQueryRequest struct {
	Mode     string `json:"mode"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	Database string `json:"database"`
	Query    string `json:"query"`
	Params   any    `json:"params"`
	SSLMode  string `json:"sslmode"`
}

type mysqlQueryRequest struct {
	Mode     string `json:"mode"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	Database string `json:"database"`
	Query    string `json:"query"`
	Params   any    `json:"params"`
	TLS      string `json:"tls"`
	Charset  string `json:"charset"`
}

func handleDataProxyMssqlQueryWithConfig(w http.ResponseWriter, req *http.Request, cfg DataProxyHandlerConfig) error {
	cfg = normalizeDataProxyHandlerConfig(cfg)

	var payload mssqlQueryRequest
	if err := decodeDataProxyJSON(w, req, cfg.RequestBodyLimitBytes, &payload); err != nil {
		errorJSON(w, "invalid JSON body", http.StatusBadRequest)
		return nil
	}
	if !hasRequiredSQLFields(payload.Server, payload.User, payload.Password, payload.Query) {
		errorJSON(w, "Missing required fields: server, user, password, query", http.StatusBadRequest)
		return nil
	}
	mode, ok := parseSQLQueryMode(payload.Mode)
	if !ok {
		errorJSON(w, `Missing or invalid mode. Expected "read" or "modify"`, http.StatusBadRequest)
		return nil
	}

	endpoint, err := resolveSQLEndpoint(req.Context(), cfg, strings.TrimSpace(payload.Server), payload.effectivePort())
	if err != nil {
		errorJSON(w, err.Error(), http.StatusBadGateway)
		return nil
	}

	db, err := sql.Open("sqlserver", payload.connectionStringFor(endpoint.Host, endpoint.Port))
	if err != nil {
		writeMssqlError(w, err)
		return nil
	}
	defer db.Close()

	connectCtx, cancelConnect := context.WithTimeout(req.Context(), defaultMssqlConnectTimeout)
	defer cancelConnect()
	if err := db.PingContext(connectCtx); err != nil {
		writeMssqlError(w, err)
		return nil
	}

	if err := executeSQLQuery(
		req.Context(),
		db,
		w,
		payload.Query,
		payload.namedArgs(),
		defaultMssqlQueryTimeout,
		mode,
	); err != nil {
		writeMssqlError(w, err)
	}
	return nil
}

func handleDataProxyPostgresQueryWithConfig(w http.ResponseWriter, req *http.Request, cfg DataProxyHandlerConfig) error {
	cfg = normalizeDataProxyHandlerConfig(cfg)

	var payload postgresQueryRequest
	if err := decodeDataProxyJSON(w, req, cfg.RequestBodyLimitBytes, &payload); err != nil {
		errorJSON(w, "invalid JSON body", http.StatusBadRequest)
		return nil
	}
	if !hasRequiredSQLFields(payload.Host, payload.User, payload.Password, payload.Query) {
		errorJSON(w, "Missing required fields: host, user, password, query", http.StatusBadRequest)
		return nil
	}
	mode, ok := parseSQLQueryMode(payload.Mode)
	if !ok {
		errorJSON(w, `Missing or invalid mode. Expected "read" or "modify"`, http.StatusBadRequest)
		return nil
	}

	args, err := positionalArgs(payload.Params)
	if err != nil {
		errorJSON(w, err.Error(), http.StatusBadRequest)
		return nil
	}

	endpoint, err := resolveSQLEndpoint(req.Context(), cfg, strings.TrimSpace(payload.Host), payload.effectivePort())
	if err != nil {
		errorJSON(w, err.Error(), http.StatusBadGateway)
		return nil
	}

	db, err := sql.Open("postgres", payload.connectionStringFor(endpoint.Host, endpoint.Port))
	if err != nil {
		writePostgresError(w, err)
		return nil
	}
	defer db.Close()

	connectCtx, cancelConnect := context.WithTimeout(req.Context(), defaultPostgresConnectMS)
	defer cancelConnect()
	if err := db.PingContext(connectCtx); err != nil {
		writePostgresError(w, err)
		return nil
	}

	if err := executeSQLQuery(
		req.Context(),
		db,
		w,
		payload.Query,
		args,
		defaultPostgresQueryMS,
		mode,
	); err != nil {
		writePostgresError(w, err)
	}
	return nil
}

func handleDataProxyMySQLQueryWithConfig(w http.ResponseWriter, req *http.Request, cfg DataProxyHandlerConfig) error {
	cfg = normalizeDataProxyHandlerConfig(cfg)

	var payload mysqlQueryRequest
	if err := decodeDataProxyJSON(w, req, cfg.RequestBodyLimitBytes, &payload); err != nil {
		errorJSON(w, "invalid JSON body", http.StatusBadRequest)
		return nil
	}
	if !hasRequiredSQLFields(payload.Host, payload.User, payload.Password, payload.Query) {
		errorJSON(w, "Missing required fields: host, user, password, query", http.StatusBadRequest)
		return nil
	}
	mode, ok := parseSQLQueryMode(payload.Mode)
	if !ok {
		errorJSON(w, `Missing or invalid mode. Expected "read" or "modify"`, http.StatusBadRequest)
		return nil
	}

	args, err := positionalArgs(payload.Params)
	if err != nil {
		errorJSON(w, err.Error(), http.StatusBadRequest)
		return nil
	}

	endpoint, err := resolveSQLEndpoint(req.Context(), cfg, strings.TrimSpace(payload.Host), payload.effectivePort())
	if err != nil {
		errorJSON(w, err.Error(), http.StatusBadGateway)
		return nil
	}

	dsn, err := payload.connectionStringFor(endpoint.Host, endpoint.Port)
	if err != nil {
		errorJSON(w, err.Error(), http.StatusBadRequest)
		return nil
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		writeMySQLError(w, err)
		return nil
	}
	defer db.Close()

	connectCtx, cancelConnect := context.WithTimeout(req.Context(), defaultMySQLConnectMS)
	defer cancelConnect()
	if err := db.PingContext(connectCtx); err != nil {
		writeMySQLError(w, err)
		return nil
	}

	if err := executeSQLQuery(
		req.Context(),
		db,
		w,
		payload.Query,
		args,
		defaultMySQLQueryMS,
		mode,
	); err != nil {
		writeMySQLError(w, err)
	}
	return nil
}

func hasRequiredSQLFields(host, user, password, query string) bool {
	return strings.TrimSpace(host) != "" &&
		strings.TrimSpace(user) != "" &&
		strings.TrimSpace(password) != "" &&
		strings.TrimSpace(query) != ""
}

type sqlEndpoint struct {
	Host string
	Port int
}

func resolveSQLEndpoint(ctx context.Context, cfg DataProxyHandlerConfig, host string, port int) (sqlEndpoint, error) {
	if cfg.TunnelManager == nil {
		return sqlEndpoint{Host: host, Port: port}, nil
	}
	return cfg.TunnelManager.EnsureTunnel(ctx, host, port)
}

func positionalArgs(raw any) ([]any, error) {
	switch typed := raw.(type) {
	case nil:
		return nil, nil
	case []any:
		for index, value := range typed {
			if number, ok := value.(float64); ok && number == float64(int64(number)) {
				typed[index] = int64(number)
			}
		}
		return typed, nil
	default:
		return nil, errors.New("params must be a JSON array for postgres/mysql queries")
	}
}

func decodeDataProxyJSON(w http.ResponseWriter, req *http.Request, maxBytes int64, target any) error {
	if maxBytes > 0 {
		req.Body = http.MaxBytesReader(w, req.Body, maxBytes)
	}
	decoder := json.NewDecoder(req.Body)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func executeSQLQuery(
	ctx context.Context,
	db *sql.DB,
	w http.ResponseWriter,
	query string,
	args []any,
	timeout time.Duration,
	mode sqlQueryMode,
) error {
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if mode == sqlQueryModeModify {
		result, err := db.ExecContext(queryCtx, query, args...)
		if err != nil {
			return err
		}
		response := sqlQueryResponse{}
		if affected, err := result.RowsAffected(); err == nil {
			response.RowsAffected = []int64{affected}
		}
		writeJSON(w, http.StatusOK, response)
		return nil
	}

	// Read mode runs inside a transaction that is always rolled back.
	// This provides a safety net when callers cannot guarantee read-only credentials.
	tx, err := beginReadTransaction(func(opts *sql.TxOptions) (*sql.Tx, error) {
		return db.BeginTx(queryCtx, opts)
	})
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	rows, err := tx.QueryContext(queryCtx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return err
	}

	recordset, err := readSQLRecordset(rows, columns)
	if err != nil {
		return err
	}

	writeJSON(w, http.StatusOK, sqlQueryResponse{Recordset: recordset})
	return nil
}

func beginReadTransaction(begin func(opts *sql.TxOptions) (*sql.Tx, error)) (*sql.Tx, error) {
	tx, err := begin(&sql.TxOptions{ReadOnly: true})
	if err == nil {
		return tx, nil
	}
	if !isReadOnlyTransactionUnsupportedError(err) {
		return nil, err
	}
	return begin(nil)
}

func isReadOnlyTransactionUnsupportedError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "read transactions are not supported") ||
		strings.Contains(lower, "read-only transactions are not supported") ||
		strings.Contains(lower, "read only transactions are not supported")
}

func parseSQLQueryMode(raw string) (sqlQueryMode, bool) {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case string(sqlQueryModeRead):
		return sqlQueryModeRead, true
	case string(sqlQueryModeModify):
		return sqlQueryModeModify, true
	default:
		return "", false
	}
}

func readSQLRecordset(rows *sql.Rows, columns []string) ([]map[string]any, error) {
	recordset := make([]map[string]any, 0)
	if len(columns) == 0 {
		return recordset, nil
	}

	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}

		row := make(map[string]any, len(columns))
		for i, column := range columns {
			value := values[i]
			if bytes, ok := value.([]byte); ok {
				row[column] = string(bytes)
				continue
			}
			row[column] = value
		}

		recordset = append(recordset, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Ignore any additional result sets for now and drain them to completion.
	for rows.NextResultSet() {
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	return recordset, nil
}

func (r mssqlQueryRequest) effectivePort() int {
	if r.Port > 0 {
		return r.Port
	}
	return defaultMssqlPort
}

func (r mssqlQueryRequest) effectiveDatabase() string {
	if strings.TrimSpace(r.Database) != "" {
		return r.Database
	}
	return defaultMssqlDatabase
}

func (r mssqlQueryRequest) encryptEnabled() bool {
	if r.Encrypt != nil {
		return *r.Encrypt
	}
	return true
}

func (r mssqlQueryRequest) trustServerCert() bool {
	if r.TrustServerCertificate != nil {
		return *r.TrustServerCertificate
	}
	return true
}

func (r mssqlQueryRequest) connectionString() string {
	return r.connectionStringFor(strings.TrimSpace(r.Server), r.effectivePort())
}

func (r mssqlQueryRequest) connectionStringFor(host string, port int) string {
	query := neturl.Values{}
	query.Set("database", r.effectiveDatabase())
	query.Set("encrypt", strconv.FormatBool(r.encryptEnabled()))
	query.Set("TrustServerCertificate", strconv.FormatBool(r.trustServerCert()))
	query.Set("connection timeout", strconv.Itoa(maxInt(1, int(defaultMssqlConnectTimeout.Seconds()))))

	return (&neturl.URL{
		Scheme:   "sqlserver",
		User:     neturl.UserPassword(r.User, r.Password),
		Host:     net.JoinHostPort(host, strconv.Itoa(port)),
		RawQuery: query.Encode(),
	}).String()
}

func (r mssqlQueryRequest) namedArgs() []any {
	if len(r.Params) == 0 {
		return nil
	}

	keys := make([]string, 0, len(r.Params))
	for key := range r.Params {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	args := make([]any, 0, len(keys))
	for _, key := range keys {
		name := strings.TrimPrefix(strings.TrimSpace(key), "@")
		if name == "" {
			continue
		}
		args = append(args, sql.Named(name, r.Params[key]))
	}
	return args
}

func (r postgresQueryRequest) effectivePort() int {
	if r.Port > 0 {
		return r.Port
	}
	return defaultPostgresPort
}

func (r postgresQueryRequest) effectiveDatabase() string {
	if strings.TrimSpace(r.Database) != "" {
		return r.Database
	}
	return defaultPostgresDatabase
}

func (r postgresQueryRequest) effectiveSSLMode() string {
	if strings.TrimSpace(r.SSLMode) != "" {
		return strings.TrimSpace(r.SSLMode)
	}
	return defaultPostgresSSLMode
}

func (r postgresQueryRequest) connectionString() string {
	return r.connectionStringFor(strings.TrimSpace(r.Host), r.effectivePort())
}

func (r postgresQueryRequest) connectionStringFor(host string, port int) string {
	query := neturl.Values{}
	query.Set("sslmode", r.effectiveSSLMode())
	query.Set("connect_timeout", strconv.Itoa(maxInt(1, int(defaultPostgresConnectMS.Seconds()))))

	return (&neturl.URL{
		Scheme:   "postgres",
		User:     neturl.UserPassword(r.User, r.Password),
		Host:     net.JoinHostPort(host, strconv.Itoa(port)),
		Path:     "/" + r.effectiveDatabase(),
		RawQuery: query.Encode(),
	}).String()
}

func (r mysqlQueryRequest) effectivePort() int {
	if r.Port > 0 {
		return r.Port
	}
	return defaultMySQLPort
}

func (r mysqlQueryRequest) effectiveDatabase() string {
	if strings.TrimSpace(r.Database) != "" {
		return r.Database
	}
	return ""
}

func (r mysqlQueryRequest) effectiveTLS() string {
	if strings.TrimSpace(r.TLS) != "" {
		return strings.TrimSpace(r.TLS)
	}
	return defaultMySQLTLS
}

func (r mysqlQueryRequest) effectiveCharset() string {
	if strings.TrimSpace(r.Charset) != "" {
		return strings.TrimSpace(r.Charset)
	}
	return defaultMySQLCharset
}

func (r mysqlQueryRequest) connectionString() (string, error) {
	return r.connectionStringFor(strings.TrimSpace(r.Host), r.effectivePort())
}

func (r mysqlQueryRequest) connectionStringFor(host string, port int) (string, error) {
	cfg := mysqlDriver.NewConfig()
	cfg.User = r.User
	cfg.Passwd = r.Password
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(host, strconv.Itoa(port))
	cfg.DBName = r.effectiveDatabase()
	cfg.ParseTime = true
	cfg.Timeout = defaultMySQLConnectMS
	cfg.ReadTimeout = defaultMySQLQueryMS
	cfg.WriteTimeout = defaultMySQLQueryMS
	cfg.TLSConfig = r.effectiveTLS()
	cfg.Params = map[string]string{
		"charset": r.effectiveCharset(),
	}

	return cfg.FormatDSN(), nil
}

func writeMssqlError(w http.ResponseWriter, err error) {
	body := baseSQLErrorBody(err)

	var sqlErr mssql.Error
	if errors.As(err, &sqlErr) {
		body["code"] = "MSSQL_ERROR"
		body["number"] = sqlErr.Number
	}

	status := inferSQLErrorStatus(err, body)
	writeJSON(w, status, body)
}

func writePostgresError(w http.ResponseWriter, err error) {
	body := baseSQLErrorBody(err)

	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		body["code"] = string(pqErr.Code)
	}

	status := inferSQLErrorStatus(err, body)
	writeJSON(w, status, body)
}

func writeMySQLError(w http.ResponseWriter, err error) {
	body := baseSQLErrorBody(err)

	var mysqlErr *mysqlDriver.MySQLError
	if errors.As(err, &mysqlErr) {
		body["code"] = "MYSQL_ERROR"
		body["number"] = mysqlErr.Number
	}

	status := inferSQLErrorStatus(err, body)
	writeJSON(w, status, body)
}

func baseSQLErrorBody(err error) map[string]any {
	if err == nil {
		return map[string]any{"error": "SQL error"}
	}
	return map[string]any{"error": err.Error()}
}

func inferSQLErrorStatus(err error, body map[string]any) int {
	status := http.StatusInternalServerError

	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
		body["code"] = "ETIMEOUT"
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		status = http.StatusGatewayTimeout
		body["code"] = "ETIMEOUT"
	}

	lower := strings.ToLower(err.Error())
	if errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(lower, "connection refused") {
		status = http.StatusServiceUnavailable
		if _, exists := body["code"]; !exists {
			body["code"] = "ECONNREFUSED"
		}
	}
	if strings.Contains(lower, "no such host") || strings.Contains(lower, "name or service not known") {
		status = http.StatusServiceUnavailable
		if _, exists := body["code"]; !exists {
			body["code"] = "ENOTFOUND"
		}
	}

	return status
}

package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

func readCodexStateMessages(ctx context.Context, stateDBPath, camelThreadID, codexSessionID string) ([]parsedChatMessage, error) {
	stateDBPath = strings.TrimSpace(stateDBPath)
	if stateDBPath == "" {
		return nil, errors.New("codex state db path is required")
	}

	db, err := sql.Open("sqlite", "file:"+stateDBPath+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	hasThreads, err := codexStateHasThreadsTable(ctx, db)
	if err != nil {
		return nil, err
	}
	if !hasThreads {
		return nil, nil
	}

	rolloutPath, err := selectCodexRolloutPath(ctx, db, codexSessionID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(rolloutPath) == "" {
		return nil, errors.New("codex state db has no rollout path")
	}

	content, err := os.ReadFile(rolloutPath)
	if err != nil && filepath.IsAbs(rolloutPath) {
		if mappedPath := mapCodexRolloutPathToStateDir(stateDBPath, rolloutPath); mappedPath != rolloutPath {
			content, err = os.ReadFile(mappedPath)
		}
	}
	if err != nil {
		return nil, err
	}
	return parseCodexRolloutMessages(string(content), camelThreadID), nil
}

func codexStateHasThreadsTable(ctx context.Context, db *sql.DB) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'threads'").Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func selectCodexRolloutPath(ctx context.Context, db *sql.DB, codexSessionID string) (string, error) {
	codexSessionID = strings.TrimSpace(codexSessionID)
	if codexSessionID != "" {
		var rolloutPath string
		err := db.QueryRowContext(ctx, "SELECT rollout_path FROM threads WHERE id = ? LIMIT 1", codexSessionID).Scan(&rolloutPath)
		if err == nil {
			return rolloutPath, nil
		}
		if err != sql.ErrNoRows {
			return "", err
		}
	}

	var rolloutPath string
	err := db.QueryRowContext(ctx, "SELECT rollout_path FROM threads ORDER BY updated_at DESC, created_at DESC LIMIT 1").Scan(&rolloutPath)
	if err != nil {
		return "", err
	}
	return rolloutPath, nil
}

func mapCodexRolloutPathToStateDir(stateDBPath, rolloutPath string) string {
	const marker = "/.codex/threads/"
	index := strings.Index(rolloutPath, marker)
	if index < 0 {
		return rolloutPath
	}
	suffix := strings.TrimPrefix(rolloutPath[index+len(marker):], "/")
	stateThreadsDir := filepath.Dir(filepath.Dir(stateDBPath))
	return filepath.Join(stateThreadsDir, filepath.FromSlash(suffix))
}

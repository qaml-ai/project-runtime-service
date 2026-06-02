package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const hostPiMigrationFileSuffix = "_legacy_migration.jsonl"
const hostPiMigrationAutoCompactThresholdBytes = 768 * 1024
const hostPiMigrationAutoCompactKeepTailBytes = 64 * 1024
const hostPiMigrationMaxToolResultTextChars = 8192
const hostPiMigrationMaxToolArgumentStringChars = 2048
const hostPiMigrationMaxToolArgumentJSONBytes = 32768
const hostPiMigrationMaxTextBlockChars = 4096

func (s *Server) migrateLegacyThreadToHostPiSession(containerName, threadID, sessionDir, workspacePath string, sessionEnv map[string]string) (int, error) {
	threadID = strings.TrimSpace(threadID)
	sessionDir = strings.TrimSpace(sessionDir)
	if threadID == "" || sessionDir == "" {
		log.Printf("[SandboxHost] host Pi legacy migration skipped thread=%s reason=missing_thread_or_session_dir sessionDir=%s", threadID, sessionDir)
		return 0, nil
	}
	if strings.ContainsAny(threadID, `/\`) {
		return 0, fmt.Errorf("invalid thread id")
	}

	hasPiSession, err := hostPiSessionDirHasJSONL(sessionDir)
	if err != nil {
		return 0, err
	}
	if hasPiSession {
		log.Printf("[SandboxHost] host Pi legacy migration skipped thread=%s reason=pi_session_exists sessionDir=%s", threadID, sessionDir)
		return 0, nil
	}

	messages, source, err := s.readLegacyMessagesForHostPiMigration(containerName, threadID, sessionEnv)
	if err != nil {
		return 0, err
	}
	if len(messages) == 0 {
		log.Printf("[SandboxHost] host Pi legacy migration found no legacy messages thread=%s container=%s", threadID, containerName)
		return 0, nil
	}

	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return 0, err
	}
	content, err := buildHostPiMigrationJSONL(threadID, workspacePath, source, messages)
	if err != nil {
		log.Printf("[SandboxHost] host Pi legacy migration build failed thread=%s source=%s messages=%d: %v", threadID, source, len(messages), err)
		return 0, err
	}
	filename := hostPiMigrationFilename(messages)
	target := filepath.Join(sessionDir, filename)
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		return 0, err
	}
	log.Printf("[SandboxHost] migrated legacy chat history to host Pi thread=%s source=%s messages=%d file=%s", threadID, source, len(messages), target)
	return len(messages), nil
}

func hostPiSessionDirHasJSONL(sessionDir string) (bool, error) {
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) readLegacyMessagesForHostPiMigration(containerName, threadID string, sessionEnv map[string]string) ([]parsedChatMessage, string, error) {
	sessionIDs, err := legacyClaudeSessionCandidates(threadID, sessionEnv["CHIRIDION_CLAUDE_SESSION_ID"])
	if err != nil {
		return nil, "", err
	}

	log.Printf("[SandboxHost] host Pi legacy migration scanning Claude history thread=%s container=%s candidateSessions=%d", threadID, containerName, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		jsonlPath := fmt.Sprintf("/home/claude/.claude/projects/-home-claude/%s.jsonl", sessionID)
		info, err := s.fs.ReadInfo(containerName, jsonlPath)
		if err != nil {
			if isNotFoundError(err) {
				log.Printf("[SandboxHost] host Pi legacy migration Claude candidate missing thread=%s session=%s path=%s", threadID, sessionID, jsonlPath)
				continue
			}
			log.Printf("[SandboxHost] host Pi legacy migration Claude candidate stat failed thread=%s session=%s path=%s: %v", threadID, sessionID, jsonlPath, err)
			return nil, "", err
		}
		content, err := os.ReadFile(info.HostPath)
		if err != nil {
			if os.IsNotExist(err) {
				log.Printf("[SandboxHost] host Pi legacy migration Claude candidate host file missing thread=%s session=%s containerPath=%s hostPath=%s", threadID, sessionID, jsonlPath, info.HostPath)
				continue
			}
			log.Printf("[SandboxHost] host Pi legacy migration Claude candidate read failed thread=%s session=%s containerPath=%s hostPath=%s: %v", threadID, sessionID, jsonlPath, info.HostPath, err)
			return nil, "", err
		}
		messages := parseClaudeJSONLMessages(string(content), threadID)
		if len(messages) > 0 {
			log.Printf("[SandboxHost] host Pi legacy migration selected Claude history thread=%s session=%s path=%s bytes=%d messages=%d", threadID, sessionID, jsonlPath, len(content), len(messages))
			return messages, "claude", nil
		}
		log.Printf("[SandboxHost] host Pi legacy migration Claude candidate parsed empty thread=%s session=%s path=%s bytes=%d", threadID, sessionID, jsonlPath, len(content))
	}

	codexSessionID := strings.TrimSpace(sessionEnv["CHIRIDION_CODEX_SESSION_ID"])
	codexThreadPaths, err := legacyCodexStatePathCandidates(threadID, codexSessionID)
	if err != nil {
		return nil, "", err
	}
	log.Printf("[SandboxHost] host Pi legacy migration scanning Codex history thread=%s container=%s codexSession=%s candidatePaths=%d", threadID, containerName, codexSessionID, len(codexThreadPaths))
	for _, codexThreadPath := range codexThreadPaths {
		info, err := s.fs.ReadInfo(containerName, codexThreadPath)
		if err != nil {
			if isNotFoundError(err) {
				log.Printf("[SandboxHost] host Pi legacy migration Codex candidate missing thread=%s path=%s", threadID, codexThreadPath)
				continue
			}
			log.Printf("[SandboxHost] host Pi legacy migration Codex candidate stat failed thread=%s path=%s: %v", threadID, codexThreadPath, err)
			return nil, "", err
		}
		messages, err := readCodexStateMessages(
			context.Background(),
			info.HostPath,
			threadID,
			codexSessionID,
		)
		if err != nil {
			log.Printf("[SandboxHost] host Pi legacy migration Codex candidate read failed thread=%s path=%s hostPath=%s codexSession=%s: %v", threadID, codexThreadPath, info.HostPath, codexSessionID, err)
			return nil, "", err
		}
		if len(messages) > 0 {
			log.Printf("[SandboxHost] host Pi legacy migration selected Codex history thread=%s path=%s hostPath=%s codexSession=%s messages=%d", threadID, codexThreadPath, info.HostPath, codexSessionID, len(messages))
			return messages, "codex", nil
		}
		log.Printf("[SandboxHost] host Pi legacy migration Codex candidate parsed empty thread=%s path=%s hostPath=%s codexSession=%s", threadID, codexThreadPath, info.HostPath, codexSessionID)
	}

	return nil, "", nil
}

func legacyClaudeSessionCandidates(threadID, claudeSessionID string) ([]string, error) {
	threadID = strings.TrimSpace(threadID)
	claudeSessionID = strings.TrimSpace(claudeSessionID)
	if threadID == "" {
		return nil, fmt.Errorf("thread id required for legacy Claude history")
	}
	if strings.ContainsAny(threadID, `/\`) {
		return nil, fmt.Errorf("invalid thread id")
	}

	sessionIDs := []string{threadID}
	if claudeSessionID != "" {
		if strings.ContainsAny(claudeSessionID, `/\`) {
			return nil, fmt.Errorf("invalid legacy Claude session id")
		}
		if claudeSessionID != threadID {
			sessionIDs = append(sessionIDs, claudeSessionID)
		}
	}
	return sessionIDs, nil
}

func legacyCodexStatePathCandidates(threadID, codexSessionID string) ([]string, error) {
	threadID = strings.TrimSpace(threadID)
	codexSessionID = strings.TrimSpace(codexSessionID)
	if threadID == "" {
		return nil, fmt.Errorf("thread id required for legacy Codex history")
	}
	if strings.ContainsAny(threadID, `/\`) {
		return nil, fmt.Errorf("invalid thread id")
	}

	ids := []string{threadID}
	if codexSessionID != "" {
		if strings.ContainsAny(codexSessionID, `/\`) {
			return nil, fmt.Errorf("invalid legacy Codex session id")
		}
		if codexSessionID != threadID {
			ids = append(ids, codexSessionID)
		}
	}

	paths := make([]string, 0, len(ids))
	for _, id := range ids {
		paths = append(paths, fmt.Sprintf("/home/claude/.codex/threads/%s/state_5.sqlite", id))
	}
	return paths, nil
}

func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "no such file") || strings.Contains(lower, "not exist")
}

func buildHostPiMigrationJSONL(threadID, workspacePath, source string, messages []parsedChatMessage) (string, error) {
	startedAt := hostPiMigrationStartedAt(messages)
	sessionID := "migrated-" + threadID
	parentID := any(nil)
	lines := make([]string, 0, len(messages)*2+1)

	sessionEvent := map[string]any{
		"type":      "session",
		"version":   3,
		"id":        sessionID,
		"timestamp": formatPiTimestamp(startedAt),
		"cwd":       workspacePath,
		"migratedFrom": map[string]any{
			"source":   source,
			"threadId": threadID,
		},
	}
	encoded, err := json.Marshal(sessionEvent)
	if err != nil {
		return "", err
	}
	lines = append(lines, string(encoded))

	usedEntryIDs := map[string]int{sessionID: 1}
	baseID := func(prefix string, index int, preferred string) string {
		if strings.TrimSpace(preferred) != "" {
			return preferred
		}
		return fmt.Sprintf("migration_%s_%d", prefix, index)
	}
	emittedToolCallIDs := make(map[string]bool)

	for messageIndex, message := range messages {
		switch message.Role {
		case "user":
			content := parsedContentToPiContentBlocks(message.Content)
			if len(content) == 0 {
				continue
			}
			id := uniquePiMigrationEntryID(usedEntryIDs, baseID("user", messageIndex, message.ID))
			line, err := marshalPiMigrationMessage(id, parentID, message.CreatedAt, map[string]any{
				"role":      "user",
				"content":   content,
				"timestamp": message.CreatedAt,
			})
			if err != nil {
				return "", err
			}
			lines = append(lines, line)
			parentID = id
		case "assistant":
			assistantBlocks, ok := asSlice(message.Content)
			if !ok {
				content := parsedContentToPiContentBlocks(message.Content)
				if len(content) == 0 {
					continue
				}
				id := uniquePiMigrationEntryID(usedEntryIDs, baseID("assistant", messageIndex, message.ID))
				line, err := marshalPiMigrationMessage(id, parentID, message.CreatedAt, piMigrationAssistantMessage(content, message.CreatedAt))
				if err != nil {
					return "", err
				}
				lines = append(lines, line)
				parentID = id
				continue
			}

			pendingAssistant := make([]any, 0)
			assistantPart := 0
			messageBaseID := baseID("assistant", messageIndex, message.ID)
			flushAssistant := func() error {
				if len(pendingAssistant) == 0 {
					return nil
				}
				id := messageBaseID
				if assistantPart > 0 {
					id = fmt.Sprintf("%s_part_%d", messageBaseID, assistantPart)
				}
				id = uniquePiMigrationEntryID(usedEntryIDs, id)
				assistantPart++
				line, err := marshalPiMigrationMessage(id, parentID, message.CreatedAt, piMigrationAssistantMessage(pendingAssistant, message.CreatedAt))
				if err != nil {
					return err
				}
				lines = append(lines, line)
				parentID = id
				pendingAssistant = make([]any, 0)
				return nil
			}

			for blockIndex, rawBlock := range assistantBlocks {
				block, ok := asMap(rawBlock)
				if !ok {
					continue
				}
				if firstString(block, "type") == "tool_result" {
					if err := flushAssistant(); err != nil {
						return "", err
					}
					toolUseID := firstString(block, "tool_use_id")
					if toolUseID == "" || !emittedToolCallIDs[toolUseID] {
						id := uniquePiMigrationEntryID(usedEntryIDs, fmt.Sprintf("%s_orphan_tool_result_%d", messageBaseID, blockIndex))
						line, err := marshalPiMigrationMessage(id, parentID, message.CreatedAt, map[string]any{
							"role":      "user",
							"content":   orphanLegacyToolResultContent(toolUseID, block["content"]),
							"timestamp": message.CreatedAt,
						})
						if err != nil {
							return "", err
						}
						lines = append(lines, line)
						parentID = id
						continue
					}
					id := uniquePiMigrationEntryID(usedEntryIDs, fmt.Sprintf("%s_tool_result_%d", messageBaseID, blockIndex))
					line, err := marshalPiMigrationMessage(id, parentID, message.CreatedAt, map[string]any{
						"role":       "toolResult",
						"toolCallId": toolUseID,
						"content":    parsedToolResultToPiContent(block["content"]),
						"isError":    firstBool(block, "is_error", "isError"),
						"timestamp":  message.CreatedAt,
					})
					if err != nil {
						return "", err
					}
					lines = append(lines, line)
					parentID = id
					continue
				}
				convertedBlocks := parsedBlockToPiContentBlocks(block)
				for _, convertedBlock := range convertedBlocks {
					converted, ok := asMap(convertedBlock)
					if ok && firstString(converted, "type") == "toolCall" {
						if id := firstString(converted, "id"); id != "" {
							emittedToolCallIDs[id] = true
						}
					}
				}
				pendingAssistant = append(pendingAssistant, convertedBlocks...)
			}
			if err := flushAssistant(); err != nil {
				return "", err
			}
		}
	}

	lines, err = maybeCompactHostPiMigrationJSONLLines(
		threadID,
		source,
		messages,
		lines,
		hostPiMigrationAutoCompactThresholdBytes,
		hostPiMigrationAutoCompactKeepTailBytes,
	)
	if err != nil {
		return "", err
	}

	return strings.Join(lines, "\n") + "\n", nil
}

func uniquePiMigrationEntryID(used map[string]int, desired string) string {
	desired = strings.TrimSpace(desired)
	if desired == "" {
		desired = "migration_entry"
	}
	count := used[desired]
	if count == 0 {
		used[desired] = 1
		return desired
	}
	for {
		count++
		candidate := fmt.Sprintf("%s_dup_%d", desired, count)
		if used[candidate] == 0 {
			used[desired] = count
			used[candidate] = 1
			return candidate
		}
	}
}

type piMigrationLineInfo struct {
	Index     int
	ID        string
	ParentID  any
	Type      string
	Role      string
	Timestamp string
	ByteEnd   int
}

func maybeCompactHostPiMigrationJSONLLines(threadID, source string, messages []parsedChatMessage, lines []string, thresholdBytes, keepTailBytes int) ([]string, error) {
	totalBytes := 0
	for _, line := range lines {
		totalBytes += len(line) + 1
	}
	if thresholdBytes <= 0 || totalBytes <= thresholdBytes || len(lines) < 3 {
		return lines, nil
	}
	if keepTailBytes <= 0 {
		keepTailBytes = hostPiMigrationAutoCompactKeepTailBytes
	}

	lineInfos, err := parsePiMigrationLineInfos(lines)
	if err != nil {
		return nil, err
	}
	firstKept := findPiMigrationFirstKeptLine(lineInfos, totalBytes, keepTailBytes)
	if firstKept == nil {
		return lines, nil
	}
	last := lastPiMigrationLineInfo(lineInfos)
	if last == nil {
		return lines, nil
	}

	usedEntryIDs := make(map[string]int, len(lineInfos)+1)
	for _, info := range lineInfos {
		if info.ID != "" {
			usedEntryIDs[info.ID] = 1
		}
	}
	compactionID := uniquePiMigrationEntryID(usedEntryIDs, "migration_compaction_"+sanitizePiMigrationID(threadID))
	summary := buildPiMigrationCompactionSummary(source, messages, firstKept, totalBytes, keepTailBytes)
	compaction := map[string]any{
		"type":             "compaction",
		"id":               compactionID,
		"parentId":         last.ID,
		"timestamp":        formatPiTimestamp(hostPiMigrationFinishedAt(messages)),
		"summary":          summary,
		"firstKeptEntryId": firstKept.ID,
		"tokensBefore":     estimatePiMigrationTokens(totalBytes),
		"details": map[string]any{
			"source":         source,
			"threadId":       threadID,
			"importedBytes":  totalBytes,
			"keepTailBytes":  keepTailBytes,
			"firstKeptEntry": firstKept.ID,
			"readFiles":      []string{},
			"modifiedFiles":  []string{},
		},
		"fromHook": true,
	}
	encoded, err := json.Marshal(compaction)
	if err != nil {
		return nil, err
	}
	return append(lines, string(encoded)), nil
}

func parsePiMigrationLineInfos(lines []string) ([]piMigrationLineInfo, error) {
	infos := make([]piMigrationLineInfo, 0, len(lines))
	byteEnd := 0
	for index, line := range lines {
		byteEnd += len(line) + 1
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, fmt.Errorf("parse migrated Pi line %d: %w", index+1, err)
		}
		info := piMigrationLineInfo{
			Index:     index,
			ID:        firstString(event, "id"),
			Type:      firstString(event, "type"),
			Timestamp: firstString(event, "timestamp"),
			ByteEnd:   byteEnd,
		}
		info.ParentID = event["parentId"]
		if message, ok := asMap(event["message"]); ok {
			info.Role = firstString(message, "role")
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func findPiMigrationFirstKeptLine(infos []piMigrationLineInfo, totalBytes, keepTailBytes int) *piMigrationLineInfo {
	targetByte := totalBytes - keepTailBytes
	var fallback *piMigrationLineInfo
	for index := range infos {
		info := &infos[index]
		if info.Type != "message" || info.ID == "" || info.ByteEnd < targetByte {
			continue
		}
		if fallback == nil && info.Role != "toolResult" {
			fallback = info
		}
		if info.Role == "user" {
			return info
		}
	}
	if fallback != nil {
		return fallback
	}
	for index := len(infos) - 1; index >= 0; index-- {
		info := &infos[index]
		if info.Type == "message" && info.ID != "" && info.Role != "toolResult" {
			return info
		}
	}
	return nil
}

func lastPiMigrationLineInfo(infos []piMigrationLineInfo) *piMigrationLineInfo {
	for index := len(infos) - 1; index >= 0; index-- {
		if infos[index].ID != "" {
			return &infos[index]
		}
	}
	return nil
}

func buildPiMigrationCompactionSummary(source string, messages []parsedChatMessage, firstKept *piMigrationLineInfo, totalBytes, keepTailBytes int) string {
	var userCount, assistantCount, metaCount, compactSummaryCount int
	for _, message := range messages {
		switch message.Role {
		case "user":
			userCount++
		case "assistant":
			assistantCount++
		}
		if message.IsMeta {
			metaCount++
		}
		if message.IsCompactSummary {
			compactSummaryCount++
		}
	}

	var builder strings.Builder
	builder.WriteString("This conversation was imported from a legacy ")
	builder.WriteString(source)
	builder.WriteString(" session and was automatically compacted during migration because the original transcript is too large to send as live model context.\n\n")
	builder.WriteString("The full imported transcript is preserved earlier in this Pi JSONL file for audit, export, and future migration work. Runtime context after this point should use this summary plus the recent verbatim tail beginning at entry `")
	builder.WriteString(firstKept.ID)
	builder.WriteString("`.\n\n")
	builder.WriteString("## Migration Stats\n")
	builder.WriteString(fmt.Sprintf("- Imported messages: %d user, %d assistant", userCount, assistantCount))
	if metaCount > 0 {
		builder.WriteString(fmt.Sprintf(", %d metadata", metaCount))
	}
	if compactSummaryCount > 0 {
		builder.WriteString(fmt.Sprintf(", %d legacy compact summaries", compactSummaryCount))
	}
	builder.WriteString("\n")
	builder.WriteString(fmt.Sprintf("- Imported transcript size: %d bytes\n", totalBytes))
	builder.WriteString(fmt.Sprintf("- Runtime context keeps a recent verbatim tail of approximately %d bytes\n", keepTailBytes))
	if start := hostPiMigrationStartedAt(messages); start > 0 {
		builder.WriteString("- First legacy timestamp: ")
		builder.WriteString(formatPiTimestamp(start))
		builder.WriteString("\n")
	}
	if end := hostPiMigrationFinishedAt(messages); end > 0 {
		builder.WriteString("- Last legacy timestamp: ")
		builder.WriteString(formatPiTimestamp(end))
		builder.WriteString("\n")
	}

	legacySummaries := collectPiMigrationLegacyCompactSummaries(messages, 6, 9000)
	if len(legacySummaries) > 0 {
		builder.WriteString("\n## Existing Legacy Compact Summaries\n")
		for _, summary := range legacySummaries {
			builder.WriteString("- ")
			builder.WriteString(summary)
			builder.WriteString("\n")
		}
	}

	userRequests := collectPiMigrationUserRequestSnippets(messages, 16, 6000)
	if len(userRequests) > 0 {
		builder.WriteString("\n## Recent User Requests Before The Kept Tail\n")
		for _, request := range userRequests {
			builder.WriteString("- ")
			builder.WriteString(request)
			builder.WriteString("\n")
		}
	}

	builder.WriteString("\n## Continuation Guidance\n")
	builder.WriteString("- Treat the verbatim messages after this compaction as authoritative recent context.\n")
	builder.WriteString("- Use the summary above only for older context; ask the user if exact details from the pre-compaction legacy transcript are needed.\n")
	return builder.String()
}

func collectPiMigrationLegacyCompactSummaries(messages []parsedChatMessage, limit int, maxChars int) []string {
	out := make([]string, 0)
	remaining := maxChars
	for index := len(messages) - 1; index >= 0 && len(out) < limit && remaining > 0; index-- {
		if !messages[index].IsCompactSummary {
			continue
		}
		text := truncatePiMigrationText(parsedContentText(messages[index].Content), minInt(remaining, 1500))
		if text == "" {
			continue
		}
		out = append(out, text)
		remaining -= len(text)
	}
	reverseStrings(out)
	return out
}

func collectPiMigrationUserRequestSnippets(messages []parsedChatMessage, limit int, maxChars int) []string {
	out := make([]string, 0)
	remaining := maxChars
	for index := len(messages) - 1; index >= 0 && len(out) < limit && remaining > 0; index-- {
		message := messages[index]
		if message.Role != "user" || message.IsMeta || message.IsCompactSummary {
			continue
		}
		text := truncatePiMigrationText(parsedContentText(message.Content), minInt(remaining, 450))
		if text == "" || strings.HasPrefix(text, "Legacy tool result") {
			continue
		}
		out = append(out, text)
		remaining -= len(text)
	}
	reverseStrings(out)
	return out
}

func parsedContentText(content any) string {
	if text, ok := asString(content); ok {
		return normalizePiMigrationWhitespace(text)
	}
	blocks, ok := asSlice(content)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, rawBlock := range blocks {
		block, ok := asMap(rawBlock)
		if !ok {
			continue
		}
		switch firstString(block, "type") {
		case "text":
			if text := firstString(block, "text"); text != "" {
				parts = append(parts, text)
			}
		case "tool_use":
			if name := firstString(block, "name"); name != "" {
				parts = append(parts, "Tool call: "+name)
			}
		case "tool_result":
			if text := firstString(block, "content"); text != "" {
				parts = append(parts, "Tool result: "+text)
			}
		}
	}
	return normalizePiMigrationWhitespace(strings.Join(parts, " "))
}

func normalizePiMigrationWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func truncatePiMigrationText(text string, maxChars int) string {
	text = normalizePiMigrationWhitespace(text)
	if maxChars <= 0 || text == "" {
		return ""
	}
	if len(text) <= maxChars {
		return text
	}
	if maxChars <= 1 {
		return text[:maxChars]
	}
	return strings.TrimSpace(text[:maxChars-1]) + "…"
}

func reverseStrings(values []string) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func estimatePiMigrationTokens(bytes int) int {
	if bytes <= 0 {
		return 0
	}
	return (bytes + 3) / 4
}

func sanitizePiMigrationID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "session"
	}
	var builder strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '-' || r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteRune('_')
		}
	}
	return builder.String()
}

func orphanLegacyToolResultContent(toolUseID string, content any) []any {
	prefix := "Legacy tool result"
	if strings.TrimSpace(toolUseID) != "" {
		prefix = fmt.Sprintf("Legacy tool result for unmatched tool call %s", toolUseID)
	}

	resultBlocks := parsedToolResultToPiContent(content)
	if len(resultBlocks) == 0 {
		return []any{map[string]any{"type": "text", "text": prefix}}
	}

	firstBlock, ok := asMap(resultBlocks[0])
	if ok && firstString(firstBlock, "type") == "text" {
		firstBlock["text"] = prefix + ":\n" + firstString(firstBlock, "text")
		return resultBlocks
	}

	out := make([]any, 0, len(resultBlocks)+1)
	out = append(out, map[string]any{"type": "text", "text": prefix + ":"})
	out = append(out, resultBlocks...)
	return out
}

func piMigrationAssistantMessage(content []any, createdAt int64) map[string]any {
	stopReason := "stop"
	for _, rawBlock := range content {
		block, ok := asMap(rawBlock)
		if ok && firstString(block, "type") == "toolCall" {
			stopReason = "toolUse"
			break
		}
	}

	return map[string]any{
		"role":       "assistant",
		"content":    content,
		"api":        "chiridion-legacy-migration",
		"provider":   "chiridion",
		"model":      "legacy-migrated-session",
		"usage":      piMigrationZeroUsage(),
		"stopReason": stopReason,
		"timestamp":  createdAt,
	}
}

func piMigrationZeroUsage() map[string]any {
	return map[string]any{
		"input":       0,
		"output":      0,
		"cacheRead":   0,
		"cacheWrite":  0,
		"totalTokens": 0,
		"cost": map[string]any{
			"input":      0,
			"output":     0,
			"cacheRead":  0,
			"cacheWrite": 0,
			"total":      0,
		},
	}
}

func marshalPiMigrationMessage(id string, parentID any, createdAt int64, message map[string]any) (string, error) {
	event := map[string]any{
		"type":      "message",
		"id":        id,
		"parentId":  parentID,
		"timestamp": formatPiTimestamp(createdAt),
		"message":   message,
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func parsedContentToPiContentBlocks(content any) []any {
	if text, ok := asString(content); ok {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": trimPiMigrationTextBlock(text)}}
	}
	blocks, ok := asSlice(content)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(blocks))
	for _, rawBlock := range blocks {
		block, ok := asMap(rawBlock)
		if !ok {
			continue
		}
		if firstString(block, "type") == "tool_result" {
			continue
		}
		out = append(out, parsedBlockToPiContentBlocks(block)...)
	}
	return out
}

func parsedBlockToPiContentBlocks(block map[string]any) []any {
	switch firstString(block, "type") {
	case "text":
		if text := firstString(block, "text"); text != "" {
			return []any{map[string]any{"type": "text", "text": trimPiMigrationTextBlock(text)}}
		}
	case "thinking":
		if thinking := firstString(block, "thinking"); thinking != "" {
			out := map[string]any{"type": "thinking", "thinking": trimPiMigrationTextBlock(thinking)}
			if signature := firstString(block, "signature", "thinkingSignature", "thinking_signature"); signature != "" {
				out["thinkingSignature"] = signature
			}
			return []any{out}
		}
	case "redacted_thinking":
		return nil
	case "image", "input_image", "screenshot", "document":
		return []any{map[string]any{"type": "text", "text": legacyMediaOmittedText(firstString(block, "type"))}}
	case "tool_use":
		id := firstString(block, "id")
		name := firstString(block, "name")
		if id == "" || name == "" {
			return nil
		}
		input, _ := asMap(block["input"])
		if input == nil {
			input = map[string]any{}
		}
		input = sanitizePiMigrationToolArguments(input)
		return []any{map[string]any{
			"type":      "toolCall",
			"id":        id,
			"name":      name,
			"arguments": input,
		}}
	}
	return nil
}

func parsedToolResultToPiContent(content any) []any {
	if text, ok := asString(content); ok {
		return []any{map[string]any{"type": "text", "text": trimPiMigrationToolResultText(text)}}
	}
	blocks, ok := asSlice(content)
	if ok {
		textParts := make([]string, 0, len(blocks))
		omittedMedia := 0
		for _, rawBlock := range blocks {
			block, ok := asMap(rawBlock)
			if !ok {
				continue
			}
			blockType := firstString(block, "type")
			if isPiMigrationMediaBlock(block) {
				omittedMedia++
				continue
			}
			if blockType == "text" {
				if text := firstString(block, "text"); text != "" {
					textParts = append(textParts, text)
				}
			}
		}
		if len(textParts) > 0 {
			text := trimPiMigrationToolResultText(strings.Join(textParts, "\n"))
			if omittedMedia > 0 {
				text += fmt.Sprintf("\n\n[%d legacy media attachment(s) omitted during migration.]", omittedMedia)
			}
			return []any{map[string]any{"type": "text", "text": text}}
		}
		if omittedMedia > 0 {
			return []any{map[string]any{"type": "text", "text": fmt.Sprintf("[%d legacy media attachment(s) omitted during migration.]", omittedMedia)}}
		}
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return []any{map[string]any{"type": "text", "text": trimPiMigrationToolResultText(fmt.Sprint(content))}}
	}
	return []any{map[string]any{"type": "text", "text": trimPiMigrationToolResultText(string(encoded))}}
}

func trimPiMigrationToolResultText(text string) string {
	if len(text) <= hostPiMigrationMaxToolResultTextChars {
		return text
	}
	return fmt.Sprintf("[Legacy tool output omitted during migration because it was %d characters.]", len(text))
}

func trimPiMigrationTextBlock(text string) string {
	if len(text) <= hostPiMigrationMaxTextBlockChars {
		return text
	}
	headChars := hostPiMigrationMaxTextBlockChars / 2
	tailChars := hostPiMigrationMaxTextBlockChars / 2
	return fmt.Sprintf(
		"%s\n\n[Legacy text block truncated during migration: omitted %d characters.]\n\n%s",
		strings.TrimSpace(text[:headChars]),
		len(text)-headChars-tailChars,
		strings.TrimSpace(text[len(text)-tailChars:]),
	)
}

func legacyMediaOmittedText(blockType string) string {
	if blockType == "" {
		blockType = "media"
	}
	return fmt.Sprintf("[Legacy %s attachment omitted during migration.]", blockType)
}

func isPiMigrationMediaBlock(block map[string]any) bool {
	blockType := strings.ToLower(firstString(block, "type"))
	switch blockType {
	case "image", "input_image", "screenshot", "document", "audio", "video", "file":
		return true
	}
	if _, ok := block["source"]; ok && (blockType == "image" || strings.Contains(blockType, "image")) {
		return true
	}
	return false
}

func sanitizePiMigrationToolArguments(input map[string]any) map[string]any {
	sanitized, ok := sanitizePiMigrationValue(input, 0).(map[string]any)
	if !ok {
		return map[string]any{}
	}
	encoded, err := json.Marshal(sanitized)
	if err == nil && len(encoded) <= hostPiMigrationMaxToolArgumentJSONBytes {
		return sanitized
	}
	return map[string]any{
		"legacy_arguments_omitted": fmt.Sprintf("Tool arguments omitted during migration because they were %d bytes after media/string trimming.", len(encoded)),
	}
}

func sanitizePiMigrationValue(value any, depth int) any {
	if depth > 12 {
		return "[Legacy nested value omitted during migration.]"
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			lowerKey := strings.ToLower(key)
			if lowerKey == "source" || lowerKey == "data" || lowerKey == "base64" || lowerKey == "image" || lowerKey == "image_url" {
				out[key] = "[Legacy media data omitted during migration.]"
				continue
			}
			out[key] = sanitizePiMigrationValue(child, depth+1)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, child := range typed {
			if block, ok := asMap(child); ok && isPiMigrationMediaBlock(block) {
				out = append(out, "[Legacy media attachment omitted during migration.]")
				continue
			}
			out = append(out, sanitizePiMigrationValue(child, depth+1))
		}
		return out
	case string:
		if len(typed) <= hostPiMigrationMaxToolArgumentStringChars {
			return typed
		}
		return fmt.Sprintf("[Legacy string omitted during migration because it was %d characters.]", len(typed))
	default:
		return value
	}
}

func hostPiMigrationStartedAt(messages []parsedChatMessage) int64 {
	for _, message := range messages {
		if message.CreatedAt > 0 {
			return message.CreatedAt
		}
	}
	return nowMillis()
}

func hostPiMigrationFinishedAt(messages []parsedChatMessage) int64 {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].CreatedAt > 0 {
			return messages[index].CreatedAt
		}
	}
	return nowMillis()
}

func hostPiMigrationFilename(messages []parsedChatMessage) string {
	startedAt := hostPiMigrationStartedAt(messages)
	return formatPiFilenameTimestamp(startedAt) + hostPiMigrationFileSuffix
}

func formatPiTimestamp(timestampMillis int64) string {
	if timestampMillis <= 0 {
		timestampMillis = nowMillis()
	}
	return time.UnixMilli(timestampMillis).UTC().Format(time.RFC3339Nano)
}

func formatPiFilenameTimestamp(timestampMillis int64) string {
	if timestampMillis <= 0 {
		timestampMillis = nowMillis()
	}
	return time.UnixMilli(timestampMillis).UTC().Format("2006-01-02T15-04-05-000Z")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

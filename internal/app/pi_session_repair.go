package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const hostPiMissingCostError = "Cannot read properties of undefined (reading 'input')"

func repairHostPiSessionDir(sessionDir string) (int, error) {
	sessionDir = strings.TrimSpace(sessionDir)
	if sessionDir == "" {
		return 0, nil
	}

	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	total := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		repaired, err := repairHostPiSessionFile(filepath.Join(sessionDir, entry.Name()))
		if err != nil {
			return total, err
		}
		total += repaired
	}
	return total, nil
}

func repairHostPiSessionFile(path string) (int, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	lines := bytes.Split(content, []byte("\n"))
	changed := false
	repaired := 0
	output := bytes.Buffer{}
	for i, line := range lines {
		if len(line) == 0 {
			if i < len(lines)-1 {
				output.WriteByte('\n')
			}
			continue
		}

		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			output.Write(line)
			if i < len(lines)-1 {
				output.WriteByte('\n')
			}
			continue
		}

		if repairHostPiMissingCostEvent(event) {
			nextLine, err := json.Marshal(event)
			if err != nil {
				return repaired, fmt.Errorf("marshal repaired Pi session event: %w", err)
			}
			line = nextLine
			changed = true
			repaired++
		}

		output.Write(line)
		if i < len(lines)-1 {
			output.WriteByte('\n')
		}
	}

	if !changed {
		return 0, nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return repaired, err
	}
	if err := os.WriteFile(path, output.Bytes(), info.Mode().Perm()); err != nil {
		return repaired, err
	}
	return repaired, nil
}

func repairHostPiMissingCostEvent(event map[string]any) bool {
	if firstString(event, "type") != "message" {
		return false
	}
	message, ok := asMap(event["message"])
	if !ok || firstString(message, "role") != "assistant" {
		return false
	}
	if firstString(message, "stopReason") != "error" {
		return false
	}
	if !strings.Contains(firstString(message, "errorMessage"), hostPiMissingCostError) {
		return false
	}
	if firstString(message, "api") != "openai-responses" || firstString(message, "model") != "x-ai/grok-4.3" {
		return false
	}

	content, ok := asSlice(message["content"])
	if !ok || len(content) == 0 {
		return false
	}

	stopReason := "stop"
	for _, rawBlock := range content {
		block, ok := asMap(rawBlock)
		if ok && firstString(block, "type") == "toolCall" {
			stopReason = "toolUse"
			break
		}
	}
	message["stopReason"] = stopReason
	delete(message, "errorMessage")
	return true
}

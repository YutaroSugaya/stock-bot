package advisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"stockbot/backend/internal/domain/market"
)

// summaryJSON を別に返すのは、呼び手が AdvisorRun.InputJSON へそのまま保存できるようにするため。
type PromptAssembler struct {
	PromptPath string
}

func (a *PromptAssembler) Build(summary *market.MarketSummary) ([]byte, []byte, error) {
	if a.PromptPath == "" {
		return nil, nil, errors.New("advisor: prompt path empty")
	}
	prompt, err := os.ReadFile(a.PromptPath)
	if err != nil {
		return nil, nil, fmt.Errorf("advisor: read prompt: %w", err)
	}
	summaryJSON, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("advisor: marshal summary: %w", err)
	}
	var buf bytes.Buffer
	buf.Write(prompt)
	buf.WriteString("\n\nInput JSON:\n")
	buf.Write(summaryJSON)
	buf.WriteString("\n")
	return buf.Bytes(), summaryJSON, nil
}

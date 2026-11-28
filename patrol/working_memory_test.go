package patrol

import (
	"context"
	"strings"
	"testing"

	"focusflash/agent"
)

type memoryTestModel func(agent.ModelRequest, agent.ModelOutput) error

func (model memoryTestModel) Generate(_ context.Context, request agent.ModelRequest, output agent.ModelOutput) error {
	return model(request, output)
}

func TestGenerateWorkingMemory(t *testing.T) {
	document := strings.Join([]string{
		"## Session Title\nSession",
		"## Current State\n已修正。待复核。",
		"## Task & Goals\nFinish the revision",
		"## Key Facts & Decisions\nKeep the prior decision",
		"## Files & Context\nagent.go",
		"## Errors & Corrections\nRetain the correction",
		"## Open Issues\nEarlier issue resolved",
	}, "\n")
	for _, test := range []struct {
		name  string
		draft string
		valid bool
	}{
		{name: "seven ordered sections", draft: document, valid: true},
		{name: "missing section", draft: strings.Replace(document, "## Files & Context\nagent.go\n", "", 1)},
		{name: "repeated section", draft: document + "\n## Open Issues\nAnother issue"},
		{name: "empty section", draft: strings.Replace(document, "agent.go", "", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := memoryTestModel(func(request agent.ModelRequest, output agent.ModelOutput) error {
				if len(request.Tools) != 0 || len(request.Messages) != 1 ||
					!strings.Contains(request.Messages[0].Text, "previous L1") ||
					!strings.Contains(request.Messages[0].Text, "archive source") {
					t.Fatalf("unexpected model request: %+v", request)
				}
				output.EmitText(test.draft)
				return nil
			})
			messages := []ArchivedMessage{{Source: "agent", Message: agent.Message{Role: agent.RoleAssistant, Text: "archive source"}}}
			memory, err := generateWorkingMemory(context.Background(), model, "previous L1", messages, 3)
			if test.valid {
				if err != nil || memory.L0 != "已修正。" || memory.L1 != document || memory.CoveredThrough != 3 {
					t.Fatalf("generated memory = %+v, %v", memory, err)
				}
			} else if err == nil {
				t.Fatalf("accepted malformed memory: %+v", memory)
			}
		})
	}
}

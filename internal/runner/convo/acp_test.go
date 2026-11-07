package convo

import (
	"encoding/json"
	"testing"
)

func TestACPReadModels(t *testing.T) {
	// OpenCode 1.19: models as a session config option.
	a := &ACP{}
	cur := a.readModels(json.RawMessage(`{"sessionId":"s","configOptions":[{"id":"mode","category":"mode","type":"select","currentValue":"build","options":[{"value":"build","name":"Build"}]},
		{"id":"model","name":"Model","category":"model","type":"select","currentValue":"local/test","options":[{"value":"local/test","name":"Local/Test"},{"name":"Group","options":[{"value":"local/other","name":"Local/Other"}]}]}]}`))
	if cur != "local/test" || a.modelOption != "model" || len(a.models) != 2 || a.models[1].ID != "local/other" {
		t.Fatalf("config option: cur=%q option=%q models=%+v", cur, a.modelOption, a.models)
	}
	// The older models field.
	b := &ACP{}
	cur = b.readModels(json.RawMessage(`{"sessionId":"s","models":{"currentModelId":"m1","availableModels":[{"modelId":"m1","name":"One"},{"modelId":"m2","name":"Two"}]}}`))
	if cur != "m1" || b.modelOption != "" || len(b.models) != 2 {
		t.Fatalf("models field: cur=%q option=%q models=%+v", cur, b.modelOption, b.models)
	}
}

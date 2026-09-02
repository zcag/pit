package api

import (
	"encoding/json"
	"testing"
)

// Every advertised tool must carry a schema a strict client will accept.
// Regression: list_boards took no arguments, so its properties marshalled to
// JSON null, and Claude Code rejected the whole tools/list — every tool
// disappeared because of the one that needed no input.
func TestToolSchemasAreValidJSON(t *testing.T) {
	for _, tl := range mcpTools {
		raw, err := json.Marshal(tl.InputSchema)
		if err != nil {
			t.Fatalf("%s: %v", tl.Name, err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s: %v", tl.Name, err)
		}
		if got["type"] != "object" {
			t.Errorf("%s: type = %v, want object", tl.Name, got["type"])
		}
		props, ok := got["properties"]
		if !ok {
			t.Errorf("%s: no properties key", tl.Name)
			continue
		}
		if props == nil {
			t.Errorf("%s: properties is null; must be an object", tl.Name)
			continue
		}
		if _, ok := props.(map[string]any); !ok {
			t.Errorf("%s: properties is %T, want object", tl.Name, props)
		}
		// Anything marked required must actually be declared.
		if req, ok := got["required"].([]any); ok {
			pm, _ := props.(map[string]any)
			for _, r := range req {
				if _, found := pm[r.(string)]; !found {
					t.Errorf("%s: required %q is not among properties", tl.Name, r)
				}
			}
		}
	}
}

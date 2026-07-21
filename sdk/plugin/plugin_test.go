package plugin

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/kordloom/switchtender/sdk"
)

// TestExtensionValidate confirms an extension that cannot serve panics at plugin startup: nil,
// empty, or holding an empty name or nil implementation.
func TestExtensionValidate(t *testing.T) {
	t.Parallel()
	runner := sdk.ToolRunnerFunc(
		func(context.Context, sdk.ToolSpec, io.Writer) (sdk.ToolResult, error) {
			return sdk.ToolResult{}, nil
		})
	tests := []struct {
		Name string
		Ext  *Extension
	}{ // Test 0: A nil extension is a programming error.
		{Name: "nil extension", Ext: nil},
		// Test 1: An extension that provides nothing is a programming error.
		{Name: "empty extension", Ext: &Extension{}},
		// Test 2: An empty tool name is a programming error.
		{Name: "empty tool name", Ext: &Extension{Tools: map[string]sdk.ToolRunner{"": runner}}},
		// Test 3: A nil runner is a programming error.
		{Name: "nil runner", Ext: &Extension{Tools: map[string]sdk.ToolRunner{"x": nil}}},
		// Test 4: A nil notifier is a programming error.
		{Name: "nil notifier", Ext: &Extension{Notifiers: map[string]sdk.Notifier{"x": nil}}},
		// Test 5: A nil AI factory is a programming error.
		{Name: "nil factory", Ext: &Extension{AIProviders: map[string]sdk.AIProviderFactory{"x": nil}}},
		// Test 6: A nil resolver is a programming error.
		{Name: "nil resolver", Ext: &Extension{SecretSources: map[string]sdk.SecretResolver{"x": nil}}},
		// Test 7: A nil minter is a programming error.
		{Name: "nil minter", Ext: &Extension{DynamicSecretSources: map[string]sdk.SecretMinter{"x": nil}}},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Errorf("test %d: validate(%s) did not panic", testNum, test.Name)
				}
			}()
			test.Ext.validate()
		})
	}
}

// TestExtensionValid confirms a populated extension passes validation.
func TestExtensionValid(t *testing.T) {
	t.Parallel()
	ext := &Extension{
		SecretSources: map[string]sdk.SecretResolver{
			"kind": func(context.Context, string) (string, error) { return "", nil },
		},
	}
	ext.validate()
}

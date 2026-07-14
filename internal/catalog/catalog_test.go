package catalog

import "testing"

func TestValidateEffort(t *testing.T) {
	tests := []struct {
		model, effort string
		wantErr       bool
	}{
		{"gpt-5.6-sol", "xhigh", false},
		{"gpt-5.6-sol", "max", true},
		{"gemini-3.1-pro", "medium", false},
		{"gemini-3.1-pro", "xhigh", true},
		{"grok-4.5", "high", false},
		{"grok-4.5", "xhigh", true},
		{"glm-5.2", "high", true},
		{"glm-5.2", "", false},
	}
	for _, tt := range tests {
		p, _ := Get(tt.model)
		err := ValidateEffort(p, tt.effort)
		if (err != nil) != tt.wantErr {
			t.Fatalf("%s/%s error=%v wantErr=%v", tt.model, tt.effort, err, tt.wantErr)
		}
	}
}

func TestFableIsNotAutoRouted(t *testing.T) {
	p, _ := Get("claude-fable-5")
	if p.AutoRoute {
		t.Fatal("Fable should remain manual until its runtime probe passes")
	}
}

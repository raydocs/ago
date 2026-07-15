package catalog

import (
	"fmt"
	"sort"
	"strings"
)

type Profile struct {
	ID             string   `json:"id"`
	DisplayName    string   `json:"display_name"`
	Provider       string   `json:"provider"`
	DefaultEffort  string   `json:"default_effort"`
	AllowedEfforts []string `json:"allowed_efforts"`
	Strengths      []string `json:"strengths"`
	AutoRoute      bool     `json:"auto_route"`
	ProbeStatus    string   `json:"probe_status"`
}

var profiles = []Profile{
	{ID: "opus", DisplayName: "Opus", Provider: "anthropic", DefaultEffort: "high", AllowedEfforts: []string{"low", "medium", "high", "xhigh", "max"}, Strengths: []string{"general", "plan", "judge", "review"}, AutoRoute: true, ProbeStatus: "tool-pass"},
	{ID: "sonnet", DisplayName: "Sonnet", Provider: "anthropic", DefaultEffort: "high", AllowedEfforts: []string{"low", "medium", "high", "xhigh", "max"}, Strengths: []string{"quick", "general"}, AutoRoute: true, ProbeStatus: "tool-pass"},
	{ID: "sonnet[1m]", DisplayName: "Sonnet 5 (1M)", Provider: "anthropic", DefaultEffort: "high", AllowedEfforts: []string{"low", "medium", "high", "xhigh", "max"}, Strengths: []string{"long-context"}, AutoRoute: true, ProbeStatus: "inherited-from-sonnet"},
	{ID: "claude-fable-5", DisplayName: "Claude Fable 5", Provider: "anthropic", DefaultEffort: "high", AllowedEfforts: []string{"low", "medium", "high", "xhigh", "max"}, Strengths: []string{"hard", "high-risk", "judge"}, AutoRoute: false, ProbeStatus: "timeout-75s"},
	{ID: "gpt-5.6-sol", DisplayName: "GPT 5.6 Sol", Provider: "openai", DefaultEffort: "high", AllowedEfforts: []string{"medium", "high", "xhigh"}, Strengths: []string{"complex-implementation", "architecture"}, AutoRoute: true, ProbeStatus: "tool-pass"},
	{ID: "gpt-5.6-luna", DisplayName: "GPT 5.6 Luna", Provider: "openai", DefaultEffort: "high", AllowedEfforts: []string{"medium", "high", "xhigh"}, Strengths: []string{"bounded-implementation", "tests", "repair"}, AutoRoute: true, ProbeStatus: "tool-pass"},
	{ID: "gpt-5.6-terra", DisplayName: "GPT 5.6 Terra", Provider: "openai", DefaultEffort: "high", AllowedEfforts: []string{"medium", "high", "xhigh"}, Strengths: []string{"exploration", "context"}, AutoRoute: true, ProbeStatus: "tool-pass"},
	{ID: "gemini-3.1-pro", DisplayName: "Gemini 3.1 Pro", Provider: "google", DefaultEffort: "high", AllowedEfforts: []string{"low", "medium", "high"}, Strengths: []string{"deep-research", "technical-review"}, AutoRoute: true, ProbeStatus: "tool-pass"},
	{ID: "gemini-3.5-flash", DisplayName: "Gemini 3.5 Flash", Provider: "google", DefaultEffort: "medium", AllowedEfforts: []string{"low", "medium", "high"}, Strengths: []string{"fast-research", "summarize"}, AutoRoute: true, ProbeStatus: "tool-pass"},
	{ID: "grok-4.5", DisplayName: "Grok 4.5", Provider: "xai", DefaultEffort: "high", AllowedEfforts: []string{"low", "medium", "high"}, Strengths: []string{"realtime", "external-signal", "second-opinion"}, AutoRoute: true, ProbeStatus: "tool-pass"},
	{ID: "glm-5.2", DisplayName: "GLM 5.2", Provider: "zai", DefaultEffort: "", AllowedEfforts: nil, Strengths: []string{"chinese", "backup", "independent-review"}, AutoRoute: true, ProbeStatus: "tool-pass"},
}

func All() []Profile {
	out := append([]Profile(nil), profiles...)
	return out
}

func Get(id string) (Profile, bool) {
	for _, p := range profiles {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

func ValidateEffort(p Profile, effort string) error {
	if effort == "" {
		return nil
	}
	for _, allowed := range p.AllowedEfforts {
		if effort == allowed {
			return nil
		}
	}
	if len(p.AllowedEfforts) == 0 {
		return fmt.Errorf("model %s uses provider-native thinking; omit --effort", p.ID)
	}
	allowed := append([]string(nil), p.AllowedEfforts...)
	sort.Strings(allowed)
	return fmt.Errorf("model %s does not support effort %q; allowed: %s", p.ID, effort, strings.Join(allowed, ", "))
}

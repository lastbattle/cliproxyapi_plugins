package main

import "testing"

func TestMatchingRuleAndAttempts(t *testing.T) {
	cfg, err := decodeConfig([]byte(`enabled: true
rules:
  - name: claude_to_gpt
    source_formats:
      - anthropic
    models:
      - "claude-*"
    fallback_models:
      - gpt-5.4
      - "$requested"
`))
	if err != nil {
		t.Fatalf("decodeConfig() error = %v", err)
	}
	rule, ok := matchingRule(cfg, "anthropic", "claude-sonnet-4-5")
	if !ok {
		t.Fatal("matchingRule() did not match anthropic claude request")
	}
	attempts := buildAttempts(rule, "claude-sonnet-4-5")
	// Explicit repeats in fallback_models are honored so operators can request
	// same-model retries; here the trailing $requested resolves to the primary
	// model name and is kept as a deliberate retry.
	want := []string{"claude-sonnet-4-5", "gpt-5.4", "claude-sonnet-4-5"}
	if len(attempts) != len(want) {
		t.Fatalf("attempts = %#v, want %#v", attempts, want)
	}
	for i := range want {
		if attempts[i] != want[i] {
			t.Fatalf("attempts = %#v, want %#v", attempts, want)
		}
	}
}

func TestBuildAttemptPlanKeepsRepeatedSameModelFallbacks(t *testing.T) {
	rule := fallbackRule{
		Name:           "transient_same_model_retry",
		PrimaryModel:   requestedModelToken,
		FallbackModels: []string{requestedModelToken, requestedModelToken, requestedModelToken, requestedModelToken},
	}
	plan := buildAttemptPlan(rule, "gpt-5.6-sol", false)
	want := []string{"gpt-5.6-sol", "gpt-5.6-sol", "gpt-5.6-sol", "gpt-5.6-sol", "gpt-5.6-sol"}
	if len(plan.Attempts) != len(want) {
		t.Fatalf("attempts = %#v, want %#v", plan.Attempts, want)
	}
	for i := range want {
		if plan.Attempts[i] != want[i] {
			t.Fatalf("attempts = %#v, want %#v", plan.Attempts, want)
		}
	}
}

func TestBuildAttemptPlanSkipsPrimaryDuringCooldown(t *testing.T) {
	rule := fallbackRule{
		Name:           "claude_to_gpt",
		PrimaryModel:   requestedModelToken,
		FallbackModels: []string{"gpt-5.4", requestedModelToken, "gemini-3-pro"},
	}
	plan := buildAttemptPlan(rule, "claude-sonnet-4-5", true)
	want := []string{"gpt-5.4", "gemini-3-pro"}
	if plan.Primary != "claude-sonnet-4-5" {
		t.Fatalf("Primary = %q, want claude-sonnet-4-5", plan.Primary)
	}
	if !plan.PrimarySkipped {
		t.Fatal("PrimarySkipped = false, want true")
	}
	if len(plan.Attempts) != len(want) {
		t.Fatalf("attempts = %#v, want %#v", plan.Attempts, want)
	}
	for i := range want {
		if plan.Attempts[i] != want[i] {
			t.Fatalf("attempts = %#v, want %#v", plan.Attempts, want)
		}
	}
}

func TestBuildAttemptPlanKeepsSameModelRuleDuringCooldown(t *testing.T) {
	rule := fallbackRule{
		Name:           "omp_gpt_responses_execution",
		PrimaryModel:   requestedModelToken,
		FallbackModels: []string{requestedModelToken},
	}
	plan := buildAttemptPlan(rule, "gpt-5.5", true)
	// The explicit $requested fallback is a deliberate same-model retry, so the
	// plan keeps it as a second attempt while still calling the primary first.
	want := []string{"gpt-5.5", "gpt-5.5"}
	if plan.Primary != "gpt-5.5" {
		t.Fatalf("Primary = %q, want gpt-5.5", plan.Primary)
	}
	if plan.PrimarySkipped {
		t.Fatal("PrimarySkipped = true, want false for same-model transform rule")
	}
	if len(plan.Attempts) != len(want) {
		t.Fatalf("attempts = %#v, want %#v", plan.Attempts, want)
	}
	for i := range want {
		if plan.Attempts[i] != want[i] {
			t.Fatalf("attempts = %#v, want %#v", plan.Attempts, want)
		}
	}
}

func TestMatchingRuleRejectsSourceMismatch(t *testing.T) {
	cfg, err := decodeConfig([]byte(`enabled: true
rules:
  - name: claude_only
    source_formats:
      - claude
    models:
      - "claude-*"
    fallback_models:
      - gpt-5.4
`))
	if err != nil {
		t.Fatalf("decodeConfig() error = %v", err)
	}
	if _, ok := matchingRule(cfg, "openai", "claude-sonnet-4-5"); ok {
		t.Fatal("matchingRule() matched unexpected source format")
	}
}

func TestWildcardMatch(t *testing.T) {
	cases := []struct {
		value   string
		pattern string
		want    bool
	}{
		{value: "claude-sonnet-4-5", pattern: "claude-*", want: true},
		{value: "prefix-middle-suffix", pattern: "prefix*suffix", want: true},
		{value: "prefix-middle-suffix", pattern: "middle*", want: false},
		{value: "gpt-5.4", pattern: "claude-*", want: false},
	}
	for _, tc := range cases {
		if got := wildcardMatch(tc.value, tc.pattern); got != tc.want {
			t.Fatalf("wildcardMatch(%q, %q) = %v, want %v", tc.value, tc.pattern, got, tc.want)
		}
	}
}

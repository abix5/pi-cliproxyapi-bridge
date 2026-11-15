package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// anthropicSample mirrors the real /api/oauth/usage response shape, where
// utilization is a percentage rather than a 0-1 fraction.
// The shape Anthropic returns today: the flat model-scoped fields are null and
// the real windows, including model-scoped ones like Fable, live in the
// structured array.
const anthropicLimitsSample = `{
  "five_hour": {"utilization": 27.0, "resets_at": "2026-08-09T10:30:00Z"},
  "seven_day": {"utilization": 18.0, "resets_at": "2026-08-13T18:00:00Z"},
  "seven_day_opus": null,
  "seven_day_sonnet": null,
  "limits": [
    {"kind": "session", "percent": 27, "resets_at": "2026-08-09T10:30:00Z", "scope": null},
    {"kind": "weekly_all", "percent": 18, "resets_at": "2026-08-13T18:00:00Z", "scope": null},
    {"kind": "weekly_scoped", "percent": 9, "resets_at": "2026-08-13T17:59:59Z",
     "scope": {"model": {"id": null, "display_name": "Fable"}}}
  ]
}`

// The older shape, before the structured array existed.
const anthropicFlatSample = `{
  "five_hour": {"utilization": 3.0, "resets_at": "2026-07-29T21:00:00Z"},
  "seven_day": {"utilization": 68.0, "resets_at": "2026-07-30T18:00:00Z"},
  "seven_day_opus": {"utilization": 12.5, "resets_at": "2026-07-31T00:00:00Z"}
}`

func claudeGroups(t *testing.T, body string) map[string]quotaGroup {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := map[string]quotaGroup{}
	for _, g := range applyQuotaRules(payload, defaultQuotaRules()["claude"]) {
		out[g.ID] = g
	}
	return out
}

func TestClaudeReadsModelScopedLimits(t *testing.T) {
	groups := claudeGroups(t, anthropicLimitsSample)
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3: %v", len(groups), groups)
	}
	fable, ok := groups["seven-day-fable"]
	if !ok {
		t.Fatalf("missing Fable window, got %v", groups)
	}
	if math.Abs(fable.RemainingFraction-0.91) > 1e-9 {
		t.Fatalf("fable remaining = %v, want 0.91", fable.RemainingFraction)
	}
	if fable.Label != "7d Fable" {
		t.Fatalf("fable label = %q", fable.Label)
	}
	// Pi renders the models entry, so it must mirror the group.
	if len(fable.Models) != 1 || fable.Models[0].RemainingFraction != fable.RemainingFraction {
		t.Fatalf("fable models entry not mirrored: %+v", fable.Models)
	}
	// 27% used is 73% left, not a clamped zero.
	if math.Abs(groups["five-hour"].RemainingFraction-0.73) > 1e-9 {
		t.Fatalf("session remaining = %v, want 0.73", groups["five-hour"].RemainingFraction)
	}
}

// The array wins over the flat fields, which are still present but null for
// scoped windows; reading them first would drop Fable.
func TestClaudePrefersStructuredArray(t *testing.T) {
	groups := claudeGroups(t, anthropicLimitsSample)
	if _, ok := groups["seven-day-fable"]; !ok {
		t.Fatal("scoped window lost to the flat fallback")
	}
	if math.Abs(groups["seven-day"].RemainingFraction-0.82) > 1e-9 {
		t.Fatalf("weekly remaining = %v, want 0.82", groups["seven-day"].RemainingFraction)
	}
}

// Payloads that predate the array must keep working.
func TestClaudeFallsBackToFlatWindows(t *testing.T) {
	groups := claudeGroups(t, anthropicFlatSample)
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3: %v", len(groups), groups)
	}
	// 68% used must report 32% remaining.
	if math.Abs(groups["seven-day"].RemainingFraction-0.32) > 1e-9 {
		t.Fatalf("weekly remaining = %v", groups["seven-day"].RemainingFraction)
	}
	if math.Abs(groups["seven-day-opus"].RemainingFraction-0.875) > 1e-9 {
		t.Fatalf("opus remaining = %v", groups["seven-day-opus"].RemainingFraction)
	}
}

// A window the account does not have carries a null percent; reporting it as
// full quota would be worse than omitting it.
func TestClaudeSkipsNullPercent(t *testing.T) {
	groups := claudeGroups(t, `{"limits":[
		{"kind":"session","percent":10,"resets_at":"x"},
		{"kind":"weekly_scoped","percent":null,"scope":{"model":{"display_name":"Opus"}}}
	],"five_hour":null,"seven_day":null}`)
	if len(groups) != 1 {
		t.Fatalf("want only the session window, got %v", groups)
	}
	if _, ok := groups["five-hour"]; !ok {
		t.Fatalf("session window missing: %v", groups)
	}
}

// The point of the rules: a window Anthropic adds later is picked up from its
// own description, with no code change.
func TestClaudeUnknownScopedWindowAppears(t *testing.T) {
	groups := claudeGroups(t, `{"limits":[
		{"kind":"weekly_scoped","percent":40,"resets_at":"2026-09-01T00:00:00Z",
		 "scope":{"model":{"display_name":"Some New Model"}}}
	]}`)
	g, ok := groups["seven-day-some-new-model"]
	if !ok {
		t.Fatalf("unknown scoped window dropped: %v", groups)
	}
	if g.Label != "7d Some New Model" {
		t.Fatalf("label = %q", g.Label)
	}
}

// A deployment can describe a payload this build has never seen.
func TestQuotaRulesAreConfigurable(t *testing.T) {
	var rules []quotaRule
	if err := json.Unmarshal([]byte(`[
		{"array":"buckets","id":"seven-day-{name}","label":"7d {name}",
		 "percent":"left","percent_means":"remaining","reset":"until",
		 "name_from":"model","name_fallback":"Scoped"}
	]`), &rules); err != nil {
		t.Fatalf("unmarshal rules: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(`{"buckets":[
		{"model":"Fable","left":25,"until":1788000000}
	]}`), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	groups := applyQuotaRules(payload, rules)
	if len(groups) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	// percent_means: remaining is taken as-is rather than inverted.
	if math.Abs(groups[0].RemainingFraction-0.25) > 1e-9 {
		t.Fatalf("remaining = %v, want 0.25", groups[0].RemainingFraction)
	}
	if groups[0].ID != "seven-day-fable" {
		t.Fatalf("id = %q", groups[0].ID)
	}
	// Unix seconds are accepted for resets as well as RFC 3339 text.
	if groups[0].ResetTime == "" {
		t.Fatal("reset time not parsed from unix seconds")
	}
}

func TestQuotaRulePathLookup(t *testing.T) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(`{"a":{"b":[{"c":7}]}}`), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, ok := lookupFloat(payload, "a.b.0.c"); !ok || got != 7 {
		t.Fatalf("lookupFloat = %v, %v", got, ok)
	}
	if _, ok := lookupFloat(payload, "a.b.9.c"); ok {
		t.Fatal("out-of-range index should not resolve")
	}
	if _, ok := lookupFloat(payload, "a.missing"); ok {
		t.Fatal("missing path should not resolve")
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Fable":      "fable",
		"Claude 5.5": "claude-55",
		"  Sonnet  ": "sonnet",
		"!!!":        "",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClampFraction(t *testing.T) {
	cases := map[float64]float64{
		-0.5:       0,
		1.5:        1,
		0.42:       0.42,
		math.NaN(): 0,
	}
	for in, want := range cases {
		if got := clampFraction(in); got != want {
			t.Fatalf("clampFraction(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestNormalizeProviderAndSupport(t *testing.T) {
	if normalizeProvider("Claude-Web") != "claude" {
		t.Fatal("expected claude-web to normalize to claude")
	}
	if normalizeProvider("ChatGPT") != "codex" {
		t.Fatal("expected chatgpt to normalize to codex")
	}
	if normalizeProvider("Copilot") != "github-copilot" {
		t.Fatal("expected copilot to normalize to github-copilot")
	}
	// Parity with the sidecar: these providers report real quota.
	for _, provider := range []string{"anthropic", "codex", "antigravity", "github-copilot", "xai"} {
		if !providerSupported(provider) {
			t.Fatalf("%s must be supported", provider)
		}
	}
	if providerSupported("unknown") {
		t.Fatal("unknown providers must be reported as unsupported")
	}
}

// Antigravity reports per-model quota; a group must surface the limit the
// user hits first and keep the model breakdown the sidecar exposed.
func TestGroupAntigravityModels(t *testing.T) {
	groups := groupAntigravityModels([]quotaModel{
		{ID: "gemini-3.6-flash-high", RemainingFraction: 0.8, ResetTime: "2026-08-09T12:05:17Z"},
		{ID: "gemini-3.6-flash-medium", RemainingFraction: 0.4, ResetTime: "2026-08-08T12:05:17Z"},
		{ID: "gemini-3.6-pro", RemainingFraction: 0.9, ResetTime: "2026-08-10T12:05:17Z"},
	})

	if len(groups) != 2 {
		t.Fatalf("expected flash and pro groups, got %d", len(groups))
	}

	var flash *quotaGroup
	for i := range groups {
		if groups[i].Label == "Flash Models" {
			flash = &groups[i]
		}
	}
	if flash == nil {
		t.Fatal("missing flash group")
	}
	if flash.RemainingFraction != 0.4 {
		t.Fatalf("group fraction = %v, want the lowest model fraction 0.4", flash.RemainingFraction)
	}
	if flash.ResetTime != "2026-08-08T12:05:17Z" {
		t.Fatalf("group reset = %q, want the earliest reset", flash.ResetTime)
	}
	if len(flash.Models) != 2 {
		t.Fatalf("expected the model breakdown to be preserved, got %d", len(flash.Models))
	}
}

// An account without a secondary window must not be reported as having full
// quota there: the sidecar omits the window entirely.
func TestCodexOmitsAbsentWindow(t *testing.T) {
	var payload codexUsagePayload
	if err := json.Unmarshal([]byte(`{"rate_limit":{
		"primary_window":{"used_percent":76,"reset_at":1785000000,"limit_window_seconds":18000},
		"secondary_window":{"used_percent":null}}}`), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.RateLimit.Secondary.UsedPercent != nil {
		t.Fatal("absent window should decode to a nil percentage")
	}
	if payload.RateLimit.Primary.UsedPercent == nil || *payload.RateLimit.Primary.UsedPercent != 76 {
		t.Fatal("primary window percentage was not decoded")
	}
}

// Window labels come from the reported duration, since the same slot means
// different things per account.
func TestCodexWindowLabel(t *testing.T) {
	hours5 := int64(18000)
	week := int64(7 * 24 * 60 * 60)
	cases := []struct {
		seconds *int64
		want    string
	}{
		{nil, "Primary Window"},
		{&hours5, "5h Window"},
		{&week, "7d Window"},
	}
	for _, c := range cases {
		if got := codexWindowLabel(c.seconds, "Primary"); got != c.want {
			t.Fatalf("codexWindowLabel = %q, want %q", got, c.want)
		}
	}
}

// Codex reports resets as a unix timestamp, not a formatted string.
func TestUnixToRFC3339(t *testing.T) {
	ts := int64(1785000000)
	if got := unixToRFC3339(&ts); got != "2026-07-25T17:20:00Z" {
		t.Fatalf("unixToRFC3339 = %q", got)
	}
	if got := unixToRFC3339(nil); got != "" {
		t.Fatalf("nil timestamp should render empty, got %q", got)
	}
}

// Pi renders the models array, so single-window providers must still fill it.
func TestSimpleGroupCarriesModels(t *testing.T) {
	group := simpleGroup("five-hour", "5h Session", 0.5, "2026-08-02T18:40:00Z")
	if len(group.Models) != 1 {
		t.Fatalf("expected a mirrored model entry, got %d", len(group.Models))
	}
	if group.Models[0].RemainingFraction != group.RemainingFraction {
		t.Fatal("model fraction must mirror the group")
	}
}

// The sidecar reports a disabled credential as "disabled" even when the host
// recorded a different status, and Pi renders that string directly.
func TestAccountStatusMirrorsSidecar(t *testing.T) {
	cases := []struct {
		file authFile
		want string
	}{
		{authFile{Disabled: true, Status: "error"}, "disabled"},
		{authFile{Disabled: true, Status: ""}, "disabled"},
		{authFile{Status: "active"}, "active"},
		{authFile{Status: ""}, "active"},
		{authFile{Status: "error"}, "error"},
	}
	for _, c := range cases {
		if got := accountStatus(c.file); got != c.want {
			t.Fatalf("accountStatus(%+v) = %q, want %q", c.file, got, c.want)
		}
	}
}

// Group order follows the provider's model ordering, so decoding must not go
// through a map.
func TestDecodeAntigravityModelsPreservesOrder(t *testing.T) {
	raw := []byte(`{
		"gemini-3.6-pro":   {"displayName":"Pro","quotaInfo":{"remainingFraction":0.5,"resetTime":"2026-08-09T00:00:00Z"}},
		"gemini-3.6-flash": {"displayName":"Flash","quotaInfo":{"remainingFraction":1.0}},
		"no-quota-model":   {"displayName":"Skipped"}
	}`)

	models, err := decodeAntigravityModels(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models without quota must be skipped, got %d", len(models))
	}
	if models[0].ID != "gemini-3.6-pro" || models[1].ID != "gemini-3.6-flash" {
		t.Fatalf("payload order was not preserved: %v", models)
	}
	if models[0].DisplayName != "Pro" {
		t.Fatalf("display name = %q", models[0].DisplayName)
	}
}

// The provider returns its catalogue in a different order on every call, so
// group order must come from the plugin, not the payload.
func TestAntigravityGroupOrderIsStable(t *testing.T) {
	forward := groupAntigravityModels([]quotaModel{
		{ID: "gemini-flash-a", RemainingFraction: 1},
		{ID: "gemini-pro-a", RemainingFraction: 1},
		{ID: "gemini-thinking-a", RemainingFraction: 1},
	})
	reversed := groupAntigravityModels([]quotaModel{
		{ID: "gemini-thinking-a", RemainingFraction: 1},
		{ID: "gemini-pro-a", RemainingFraction: 1},
		{ID: "gemini-flash-a", RemainingFraction: 1},
	})

	var a, b []string
	for _, g := range forward {
		a = append(a, g.ID)
	}
	for _, g := range reversed {
		b = append(b, g.ID)
	}
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("group order depends on payload order: %v vs %v", a, b)
	}
	if a[0] != "pro-models" {
		t.Fatalf("expected pro models first, got %v", a)
	}
}

// ---- xAI (Grok) ----

const xaiTestManagementKey = "test-management-key"

// fakeManagement stands in for the CLIProxyAPI management API. It answers the
// fixed /api-call passthrough with a canned upstream status and body, and
// records the request the plugin sent.
type fakeManagement struct {
	server *httptest.Server
	got    apiCallRequest
	calls  int
}

func newFakeManagement(t *testing.T, status int, body string) *fakeManagement {
	t.Helper()
	fake := &fakeManagement{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/api-call" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+xaiTestManagementKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fake.calls++
		if err := json.NewDecoder(r.Body).Decode(&fake.got); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(apiCallResponse{StatusCode: status, Body: body})
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeManagement) config(t *testing.T, advanced string) pluginConfig {
	t.Helper()
	t.Setenv("PI_BRIDGE_TEST_MANAGEMENT_KEY", xaiTestManagementKey)
	if advanced == "" {
		advanced = "{}"
	}
	var overrides map[string]any
	if err := json.Unmarshal([]byte(advanced), &overrides); err != nil {
		t.Fatalf("advanced: %v", err)
	}
	overrides["management_url"] = f.server.URL + "/v0/management"
	overrides["management_key_env"] = "PI_BRIDGE_TEST_MANAGEMENT_KEY"
	blob, err := json.Marshal(overrides)
	if err != nil {
		t.Fatalf("marshal advanced: %v", err)
	}
	raw, err := json.Marshal(map[string]string{"advanced": string(blob)})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	cfg, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	return cfg
}

const xaiBillingSample = `{
  "config": {
    "creditUsagePercent": 4.0,
    "currentPeriod": {"type": "USAGE_PERIOD_TYPE_WEEKLY",
      "start": "2026-11-10T00:00:00Z", "end": "2026-11-17T00:00:00Z"},
    "productUsage": [{"product": "grok-cli", "usagePercent": 4.0}]
  },
  "prepaidBalance": {"val": "0"},
  "isUnifiedBillingUser": false,
  "billingPeriodStart": "2026-11-01T00:00:00Z",
  "billingPeriodEnd": "2026-12-01T00:00:00Z"
}`

func xaiQuota(t *testing.T, status int, body string) ([]quotaGroup, *fakeManagement, error) {
	t.Helper()
	fake := newFakeManagement(t, status, body)
	groups, err := fetchQuota(context.Background(), fake.config(t, ""), "xai", authFile{AuthIndex: "xai-1"})
	return groups, fake, err
}

func requireXAIWeekly(t *testing.T, groups []quotaGroup, err error, remaining float64, reset string) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want one weekly group", groups)
	}
	g := groups[0]
	if g.ID != "seven-day" || g.Label != "7d Weekly" {
		t.Fatalf("group = %q/%q, want seven-day/7d Weekly", g.ID, g.Label)
	}
	if math.Abs(g.RemainingFraction-remaining) > 1e-9 {
		t.Fatalf("remaining = %v, want %v", g.RemainingFraction, remaining)
	}
	if g.ResetTime != reset {
		t.Fatalf("resetTime = %q, want %q", g.ResetTime, reset)
	}
	if len(g.Models) != 1 || g.Models[0].RemainingFraction != g.RemainingFraction {
		t.Fatalf("models entry not mirrored: %+v", g.Models)
	}
}

func TestXAIIsSupported(t *testing.T) {
	if !providerSupported("xai") || !providerSupported("XAI") {
		t.Fatal("xai must be supported")
	}
}

func TestXAIWeeklyUsage(t *testing.T) {
	groups, _, err := xaiQuota(t, http.StatusOK, xaiBillingSample)
	requireXAIWeekly(t, groups, err, 0.96, "2026-11-17T00:00:00Z")
}

// The request goes through the passthrough with only the placeholder: the
// plugin never sees the token.
func TestXAIRequestShape(t *testing.T) {
	_, fake, err := xaiQuota(t, http.StatusOK, xaiBillingSample)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := fake.got
	if fake.calls != 1 || got.AuthIndex != "xai-1" || got.Method != http.MethodGet {
		t.Fatalf("request = %+v (calls %d)", got, fake.calls)
	}
	if got.URL != "https://cli-chat-proxy.grok.com/v1/billing?format=credits" {
		t.Fatalf("url = %q", got.URL)
	}
	want := map[string]string{
		"Authorization":         "Bearer $TOKEN$",
		"X-XAI-Token-Auth":      "xai-grok-cli",
		"x-grok-client-version": "1.0.13",
		"Accept":                "application/json",
	}
	for name, value := range want {
		if got.Header[name] != value {
			t.Fatalf("header %s = %q, want %q", name, got.Header[name], value)
		}
	}
	if got.Data != "" {
		t.Fatalf("GET must carry no body, got %q", got.Data)
	}
}

func TestXAIClientVersionIsConfigurable(t *testing.T) {
	fake := newFakeManagement(t, http.StatusOK, xaiBillingSample)
	cfg := fake.config(t, `{"xai_client_version": " 1.0.70 "}`)
	if _, err := fetchQuota(context.Background(), cfg, "xai", authFile{AuthIndex: "xai-1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := fake.got.Header["x-grok-client-version"]; got != "1.0.70" {
		t.Fatalf("client version = %q, want 1.0.70", got)
	}
}

// Management Center #492: the billing API omits the percentage while nothing
// has been used.
func TestXAIOmittedPercentMeansUnused(t *testing.T) {
	groups, _, err := xaiQuota(t, http.StatusOK, `{"config":{"currentPeriod":{
		"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-11-17T00:00:00Z"}}}`)
	requireXAIWeekly(t, groups, err, 1, "2026-11-17T00:00:00Z")
}

// A period the plugin cannot describe as a weekly window must not be
// reported as full quota.
func TestXAIRejectsUnusablePeriod(t *testing.T) {
	cases := map[string]string{
		"monthly":    `{"config":{"creditUsagePercent":4,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_MONTHLY","end":"2026-12-01T00:00:00Z"}}}`,
		"no end":     `{"config":{"creditUsagePercent":4,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY"}}}`,
		"bad end":    `{"config":{"creditUsagePercent":4,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"next week"}}}`,
		"no period":  `{"config":{"creditUsagePercent":4}}`,
		"no config":  `{}`,
		"null":       `null`,
		"empty type": `{"config":{"currentPeriod":{"end":"2026-11-17T00:00:00Z"}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			groups, _, err := xaiQuota(t, http.StatusOK, body)
			if err == nil {
				t.Fatalf("want an error, got groups %+v", groups)
			}
		})
	}
}

func TestXAIClampsUsage(t *testing.T) {
	groups, _, err := xaiQuota(t, http.StatusOK, `{"config":{"creditUsagePercent":130,
		"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-11-17T00:00:00Z"}}}`)
	requireXAIWeekly(t, groups, err, 0, "2026-11-17T00:00:00Z")
}

// When the billing read itself says the account is out, that is zero
// remaining, not an unknown.
func TestXAIExhaustedIsZero(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"spending limit": {http.StatusForbidden, `{"code":"personal-team-blocked:spending-limit","error":"Your team has reached its spending limit"}`},
		"free usage":     {http.StatusTooManyRequests, `{"code":"free-usage-exhausted","error":"Free usage exhausted"}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			groups, _, err := xaiQuota(t, c.status, c.body)
			requireXAIWeekly(t, groups, err, 0, "")
		})
	}
}

func TestXAIExhaustedKeepsKnownReset(t *testing.T) {
	groups, _, err := xaiQuota(t, http.StatusForbidden, `{"code":"personal-team-blocked:spending-limit",
		"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-11-17T00:00:00Z"}}}`)
	requireXAIWeekly(t, groups, err, 0, "2026-11-17T00:00:00Z")
}

// Every other refusal is a failure to read quota, and the client sees only
// the status code, never the upstream body.
func TestXAIOtherRefusalsFail(t *testing.T) {
	const secret = "upstream-detail-must-not-leak"
	cases := map[string]struct {
		status int
		body   string
	}{
		"access denied":      {http.StatusForbidden, `{"error":"Access denied ` + secret + `"}`},
		"429 other":          {http.StatusTooManyRequests, `{"error":"slow down ` + secret + `"}`},
		"upgrade required":   {http.StatusUpgradeRequired, `{"error":"client too old ` + secret + `"}`},
		"unauthorized":       {http.StatusUnauthorized, secret},
		"spending limit 429": {http.StatusTooManyRequests, `{"code":"personal-team-blocked:spending-limit"}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			groups, _, err := xaiQuota(t, c.status, c.body)
			if err == nil {
				t.Fatalf("want a failure, got groups %+v", groups)
			}
			if want := providerFailure(c.status).Error(); err.Error() != want {
				t.Fatalf("error = %q, want %q", err, want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("upstream body leaked into the error")
			}
		})
	}
}

func TestXAIMalformedPayloadFails(t *testing.T) {
	groups, _, err := xaiQuota(t, http.StatusOK, `{"config": {"creditUsagePercent": "4`)
	if err == nil {
		t.Fatalf("want a failure, got groups %+v", groups)
	}
}

// buildUsage reports xai as a supported provider with its weekly window.
func TestXAIInUsageDocument(t *testing.T) {
	fake := newFakeManagement(t, http.StatusOK, xaiBillingSample)
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/management/auth-files", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(authFilesResponse{Files: []authFile{{AuthIndex: "xai-1", Provider: "xai"}}})
	})
	mux.Handle("/v0/management/api-call", fake.server.Config.Handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	cfg := fake.config(t, "")
	cfg.advanced.ManagementURL = server.URL + "/v0/management"
	doc, err := buildUsage(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildUsage: %v", err)
	}
	if len(doc.Unsupported) != 0 {
		t.Fatalf("unsupported = %v, want none", doc.Unsupported)
	}
	if len(doc.Accounts) != 1 {
		t.Fatalf("accounts = %+v", doc.Accounts)
	}
	account := doc.Accounts[0]
	if !account.Supported || account.Error != "" {
		t.Fatalf("account = %+v", account)
	}
	requireXAIWeekly(t, account.Groups, nil, 0.96, "2026-11-17T00:00:00Z")
}

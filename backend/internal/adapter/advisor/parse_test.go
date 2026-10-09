package advisor

import (
	"errors"
	"strings"
	"testing"
)

func TestParse_StripsYAMLFence(t *testing.T) {
	in := "```yaml\nconfig_id: x\nstrategy_name: bnf_reversion\nmode: paper_config\n```\n"
	got, err := ResponseParser{}.Parse([]byte(in))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Contains(string(got), "```") {
		t.Fatalf("fence not stripped: %q", got)
	}
	if !strings.Contains(string(got), "config_id: x") {
		t.Fatalf("body lost: %q", got)
	}
}

func TestParse_EmptyIsErrEmpty(t *testing.T) {
	if _, err := (ResponseParser{}).Parse([]byte("   \n  ")); !errors.Is(err, errEmptyStdout) {
		t.Fatalf("want errEmptyStdout, got %v", err)
	}
}

func TestParse_NoConfigIDRejected(t *testing.T) {
	if _, err := (ResponseParser{}).Parse([]byte("hello: world\n")); err == nil || errors.Is(err, errEmptyStdout) {
		t.Fatalf("want parse error, got %v", err)
	}
}

// A human-preview block (config_id + prose, no body keys) preceding the real
// config must be discarded in favour of the body-key-rich block.
func TestParse_SelectsRichBodyOverPreview(t *testing.T) {
	in := "config_id: preview\nsummary: just a human note\n\n" +
		"config_id: real\nstrategy_name: bnf_reversion\nmode: paper_config\nentry:\n  direction: buy_only\nexit:\n  take_profit_jpy: 0\nrisk:\n  quantity: 100\n"
	got, err := ResponseParser{}.Parse([]byte(in))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(string(got), "config_id: real") || strings.Contains(string(got), "config_id: preview") {
		t.Fatalf("wrong block selected: %q", got)
	}
}

func TestSanitize_ColonSpaceAndTZLabel(t *testing.T) {
	in := "config_id:x\ngenerated_at: 2026-07-21T10:00:00+09:00 (JST)\nmode: paper_config\n"
	got, err := ResponseParser{}.Parse([]byte(in))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "config_id: x") {
		t.Fatalf("colon-space not repaired: %q", s)
	}
	if strings.Contains(s, "(JST)") {
		t.Fatalf("tz label not stripped: %q", s)
	}
}

func TestClassifyCLIOutput(t *testing.T) {
	cases := map[string]cliOutcome{
		"config_id: x\nmode: paper_config": cliOutcomeOK,
		"API Error: 529 Overloaded":        cliOutcomeTransient,
		"Unable to connect to API":         cliOutcomeTransient,
		"You've hit your session limit":    cliOutcomeUsageLimit,
		// server-side 429 says "not your usage limit" — must route to TRANSIENT
		// (retry), NOT usage-limit (give up). A real live bug in an earlier project.
		"API Error: Server is temporarily limiting requests (not your usage limit) · Rate limited": cliOutcomeTransient,
	}
	for in, want := range cases {
		if got := classifyCLIOutput(in); got != want {
			t.Errorf("classify(%q)=%d want %d", in, got, want)
		}
	}
}

func TestSanitize_HTMLEntitySpaces(t *testing.T) {
	// every space replaced by &#32; (a real failure mode seen in an earlier project)
	in := "config_id:&#32;x\nmode:&#32;paper_config\n"
	got, err := ResponseParser{}.Parse([]byte(in))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(string(got), "config_id: x") || !strings.Contains(string(got), "mode: paper_config") {
		t.Fatalf("entity spaces not restored: %q", got)
	}
}

func TestSanitize_KeyDroppedColonBeforeQuote(t *testing.T) {
	in := "config_id\"20260721-x\"\nmode: paper_config\n"
	got, err := ResponseParser{}.Parse([]byte(in))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(string(got), `config_id: "20260721-x"`) {
		t.Fatalf("dropped colon not repaired: %q", got)
	}
}

// An empty ```yaml``` fence next to the real config must not be selected.
func TestParse_EmptyFenceDoesNotWinOverRealBody(t *testing.T) {
	in := "```yaml\n```\nconfig_id: real\nstrategy_name: bnf_reversion\nmode: paper_config\n"
	got, err := ResponseParser{}.Parse([]byte(in))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(string(got), "config_id: real") {
		t.Fatalf("empty fence won over real body: %q", got)
	}
}

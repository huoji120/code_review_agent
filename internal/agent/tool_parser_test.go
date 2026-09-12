package agent

import "testing"

func TestParseToolCallAcceptsQwenDSMLEnvelope(t *testing.T) {
	answer := `<think><tool_call>{"name":"report_finding","arguments":{"title":"reasoning example"}}</tool_call></think><tool_call>{"name":"review_state","arguments":{"limit":10}}</｜｜DSML｜｜ parameter>
</｜｜DSML｜｜ invoke>
</｜｜DSML｜｜ calls>`
	call, ok := parseToolCall(answer)
	if !ok {
		t.Fatal("expected the complete DSML payload to parse")
	}
	if call.Name != "review_state" {
		t.Fatalf("parsed %q, want review_state", call.Name)
	}
}

func TestParseToolCallAcceptsDSMLInvokeArguments(t *testing.T) {
	answer := `<｜｜DSML｜｜ calls>
<｜｜DSML｜｜ invoke name="review_state">
{"limit":10}
</｜｜DSML｜｜ parameter>
</｜｜DSML｜｜ invoke>
</｜｜DSML｜｜ calls>`
	call, ok := parseToolCall(answer)
	if !ok {
		t.Fatal("expected the complete DSML invoke payload to parse")
	}
	if call.Name != "review_state" || string(call.Arguments) != `{"limit":10}` {
		t.Fatalf("unexpected call: %s %s", call.Name, call.Arguments)
	}
}

func TestParseToolCallRejectsIncompleteDSML(t *testing.T) {
	answer := `<｜｜DSML｜｜ calls><｜｜DSML｜｜ invoke name="review_state">{"limit":`
	if _, ok := parseToolCall(answer); ok {
		t.Fatal("incomplete DSML must not execute")
	}
}

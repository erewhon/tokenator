package analyze

import "testing"

// ts builds a fixed-date timestamp from a minute offset, keeping test cases
// readable: ts(5) < ts(10) lexically and chronologically.
func ts(min int) string {
	return "2026-07-17T10:" + string(rune('0'+min/10)) + string(rune('0'+min%10)) + ":00.000Z"
}

func TestResidencyCounting(t *testing.T) {
	blocks := []ResBlock{
		{SessionID: 1, TS: ts(1), Kind: "tool_result", Tool: "Read", EstTokens: 100},
		{SessionID: 1, TS: ts(5), Kind: "tool_result", Tool: "Grep", EstTokens: 10},
	}
	reqs := []Stamp{
		{SessionID: 1, TS: ts(0), PromptTokens: 1000}, // before both blocks
		{SessionID: 1, TS: ts(1), PromptTokens: 1000}, // AT first block's ts: not counted (strict >)
		{SessionID: 1, TS: ts(2), PromptTokens: 1000},
		{SessionID: 1, TS: ts(6), PromptTokens: 1000},
		{SessionID: 1, TS: ts(8), PromptTokens: 1000},
	}
	rep := Residency(blocks, reqs, nil)

	if got := rep.Items[0].Resident; got != 3 { // ts 2, 6, 8
		t.Errorf("block 1 resident = %d, want 3", got)
	}
	if got := rep.Items[1].Resident; got != 2 { // ts 6, 8
		t.Errorf("block 2 resident = %d, want 2", got)
	}
	if rep.Items[0].ResTokens != 300 || rep.Items[1].ResTokens != 20 {
		t.Errorf("res tokens = %d/%d, want 300/20", rep.Items[0].ResTokens, rep.Items[1].ResTokens)
	}
	if rep.MeteredPrompt != 5000 {
		t.Errorf("metered prompt = %d, want 5000", rep.MeteredPrompt)
	}
	if rep.ExtractedRes != 320 {
		t.Errorf("extracted res = %d, want 320", rep.ExtractedRes)
	}
}

func TestResidencyCompactionBound(t *testing.T) {
	blocks := []ResBlock{
		{SessionID: 1, TS: ts(1), Kind: "tool_result", EstTokens: 100},
		{SessionID: 1, TS: ts(6), Kind: "tool_result", EstTokens: 100}, // after compaction
	}
	reqs := []Stamp{
		{SessionID: 1, TS: ts(2)},
		{SessionID: 1, TS: ts(4)},
		{SessionID: 1, TS: ts(7)}, // post-compaction: first block evicted
		{SessionID: 1, TS: ts(9)},
	}
	comps := map[int64][]string{1: {ts(5)}}
	rep := Residency(blocks, reqs, comps)

	if got := rep.Items[0].Resident; got != 2 { // ts 2, 4 only
		t.Errorf("pre-compaction block resident = %d, want 2", got)
	}
	if got := rep.Items[1].Resident; got != 2 { // ts 7, 9
		t.Errorf("post-compaction block resident = %d, want 2", got)
	}
}

func TestResidencyThinkingTurnBound(t *testing.T) {
	blocks := []ResBlock{
		{SessionID: 1, TS: ts(1), Kind: "thinking", EstTokens: 100},
		{SessionID: 1, TS: ts(4), Kind: "user_text", EstTokens: 10}, // next turn starts
		{SessionID: 1, TS: ts(1), Kind: "assistant_text", EstTokens: 50},
	}
	reqs := []Stamp{
		{SessionID: 1, TS: ts(2)},
		{SessionID: 1, TS: ts(3)},
		{SessionID: 1, TS: ts(5)}, // after next user_text: thinking stripped
		{SessionID: 1, TS: ts(7)},
	}
	rep := Residency(blocks, reqs, nil)

	if got := rep.Items[0].Resident; got != 2 { // in-turn requests only
		t.Errorf("thinking resident = %d, want 2", got)
	}
	if got := rep.Items[2].Resident; got != 4 { // assistant text stays for all
		t.Errorf("assistant_text resident = %d, want 4", got)
	}
}

func TestResidencySessionIsolation(t *testing.T) {
	blocks := []ResBlock{{SessionID: 1, TS: ts(1), Kind: "tool_result", EstTokens: 100}}
	reqs := []Stamp{
		{SessionID: 1, TS: ts(2)},
		{SessionID: 2, TS: ts(3)}, // other session must not count
		{SessionID: 2, TS: ts(4)},
	}
	rep := Residency(blocks, reqs, nil)
	if got := rep.Items[0].Resident; got != 1 {
		t.Errorf("resident = %d, want 1 (other sessions excluded)", got)
	}
}

func TestRollupResidency(t *testing.T) {
	items := []BlockResidency{
		{ResBlock: ResBlock{Kind: "tool_result", Tool: "Read", EstTokens: 100}, Resident: 10, ResTokens: 1000},
		{ResBlock: ResBlock{Kind: "tool_result", Tool: "Read", EstTokens: 300}, Resident: 1, ResTokens: 300},
		{ResBlock: ResBlock{Kind: "tool_result", Tool: "Grep", EstTokens: 50}, Resident: 50, ResTokens: 2500},
		{ResBlock: ResBlock{Kind: "assistant_text", EstTokens: 500}, Resident: 4, ResTokens: 2000}, // not a tool_result
	}
	rows, ok := RollupResidency(items, "tool")
	if !ok {
		t.Fatal("tool grouping rejected")
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Grep leads on resident tokens despite smaller flow.
	if rows[0].Group != "Grep" || rows[0].ResTokens != 2500 {
		t.Errorf("rows[0] = %+v, want Grep/2500", rows[0])
	}
	if rows[1].Group != "Read" || rows[1].FlowTokens != 400 || rows[1].ResTokens != 1300 {
		t.Errorf("rows[1] = %+v, want Read flow=400 res=1300", rows[1])
	}
	if mr := rows[1].MeanResident(); mr < 3.2 || mr > 3.3 { // 1300/400
		t.Errorf("Read mean residency = %.2f, want 3.25", mr)
	}
	if _, ok := RollupResidency(items, "bogus"); ok {
		t.Error("bogus grouping accepted")
	}
}

func TestTopResidents(t *testing.T) {
	items := []BlockResidency{
		{ResBlock: ResBlock{Tool: "a"}, ResTokens: 10},
		{ResBlock: ResBlock{Tool: "b"}, ResTokens: 30},
		{ResBlock: ResBlock{Tool: "c"}, ResTokens: 20},
	}
	top := TopResidents(items, 2)
	if len(top) != 2 || top[0].Tool != "b" || top[1].Tool != "c" {
		t.Errorf("top = %+v, want b then c", top)
	}
	if items[0].ResTokens != 10 {
		t.Error("TopResidents mutated its input")
	}
}

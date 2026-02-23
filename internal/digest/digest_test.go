package digest

import (
	"context"
	"testing"

	"github.com/hergen/toad/internal/config"
	"github.com/hergen/toad/internal/tadpole"
)

func TestDedupChannel(t *testing.T) {
	msgs := []Message{
		{Text: "error A", User: "bot", Timestamp: "1"},
		{Text: "error B", User: "bot", Timestamp: "2"},
		{Text: "error A", User: "bot", Timestamp: "3"},
		{Text: "error A", User: "bot", Timestamp: "4"},
		{Text: "error B", User: "bot", Timestamp: "5"},
	}

	result := dedupChannel(msgs)

	if len(result) != 2 {
		t.Fatalf("expected 2 deduped messages, got %d", len(result))
	}
	if result[0].Text != "error A (x3 duplicates)" {
		t.Errorf("expected 'error A (x3 duplicates)', got %q", result[0].Text)
	}
	if result[0].Timestamp != "1" {
		t.Errorf("expected first occurrence timestamp '1', got %q", result[0].Timestamp)
	}
	if result[1].Text != "error B (x2 duplicates)" {
		t.Errorf("expected 'error B (x2 duplicates)', got %q", result[1].Text)
	}
}

func TestDedupChannelNoDuplicates(t *testing.T) {
	msgs := []Message{
		{Text: "error A"},
		{Text: "error B"},
		{Text: "error C"},
	}

	result := dedupChannel(msgs)

	if len(result) != 3 {
		t.Fatalf("expected 3 messages (no dedup), got %d", len(result))
	}
	for i, msg := range result {
		if msg.Text != msgs[i].Text {
			t.Errorf("message %d: expected %q, got %q", i, msgs[i].Text, msg.Text)
		}
	}
}

func TestBuildChunks_SingleChannel(t *testing.T) {
	cfg := &config.DigestConfig{MaxChunkSize: 50}
	e := &Engine{cfg: cfg}

	msgs := make([]Message, 10)
	for i := range msgs {
		msgs[i] = Message{ChannelName: "errors", Text: "unique error " + string(rune('A'+i))}
	}

	chunks := e.buildChunks(msgs)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if len(chunks[0].messages) != 10 {
		t.Errorf("expected 10 messages in chunk, got %d", len(chunks[0].messages))
	}
	if chunks[0].label != "#errors (10 msgs)" {
		t.Errorf("unexpected label: %q", chunks[0].label)
	}
}

func TestBuildChunks_LargeChannelNeverSplit(t *testing.T) {
	cfg := &config.DigestConfig{MaxChunkSize: 5}
	e := &Engine{cfg: cfg}

	// 12 unique messages from one channel — should NOT be split
	msgs := make([]Message, 12)
	for i := range msgs {
		msgs[i] = Message{ChannelName: "errors", Text: "unique error " + string(rune('A'+i))}
	}

	chunks := e.buildChunks(msgs)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk (single channel never split), got %d", len(chunks))
	}
	if len(chunks[0].messages) != 12 {
		t.Errorf("expected all 12 messages in chunk, got %d", len(chunks[0].messages))
	}
}

func TestBuildChunks_MixedSmall(t *testing.T) {
	cfg := &config.DigestConfig{MaxChunkSize: 50}
	e := &Engine{cfg: cfg}

	var msgs []Message
	for _, ch := range []string{"general", "random", "dev"} {
		for i := 0; i < 3; i++ {
			msgs = append(msgs, Message{ChannelName: ch, Text: ch + " msg"})
		}
	}

	chunks := e.buildChunks(msgs)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 mixed chunk, got %d", len(chunks))
	}
	// 9 messages, but 3 channels * 1 unique text each = 3 deduped messages
	if len(chunks[0].messages) != 3 {
		t.Errorf("expected 3 deduped messages, got %d", len(chunks[0].messages))
	}
}

func TestBuildChunks_SmallChannelsOverflow(t *testing.T) {
	cfg := &config.DigestConfig{MaxChunkSize: 5}
	e := &Engine{cfg: cfg}

	var msgs []Message
	// 4 channels with 3 unique messages each = 12 messages, max 5 per mixed chunk
	for _, ch := range []string{"ch-a", "ch-b", "ch-c", "ch-d"} {
		for i := 0; i < 3; i++ {
			msgs = append(msgs, Message{ChannelName: ch, Text: ch + " unique " + string(rune('0'+i))})
		}
	}

	chunks := e.buildChunks(msgs)

	totalMsgs := 0
	for _, ch := range chunks {
		totalMsgs += len(ch.messages)
	}
	if totalMsgs != 12 {
		t.Errorf("expected 12 total messages across chunks, got %d", totalMsgs)
	}
	// Each channel has 3 msgs. Coalescing: ch-a(3) would fit, +ch-b(3)=6 > 5, so flush.
	// Result: at least 2 chunks (no single chunk can hold all 12 with max 5)
	if len(chunks) < 2 {
		t.Errorf("expected multiple chunks for overflow, got %d", len(chunks))
	}
}

func TestBuildChunks_DedupReducesChunks(t *testing.T) {
	cfg := &config.DigestConfig{MaxChunkSize: 5}
	e := &Engine{cfg: cfg}

	// 20 messages but all identical → should dedup to 1
	msgs := make([]Message, 20)
	for i := range msgs {
		msgs[i] = Message{ChannelName: "errors", Text: "same error"}
	}

	chunks := e.buildChunks(msgs)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk after dedup, got %d", len(chunks))
	}
	if len(chunks[0].messages) != 1 {
		t.Errorf("expected 1 deduped message, got %d", len(chunks[0].messages))
	}
	if chunks[0].messages[0].Text != "same error (x20 duplicates)" {
		t.Errorf("unexpected deduped text: %q", chunks[0].messages[0].Text)
	}
}

func TestBuildChunks_LargeAndSmallChannels(t *testing.T) {
	cfg := &config.DigestConfig{MaxChunkSize: 5}
	e := &Engine{cfg: cfg}

	var msgs []Message
	// Large channel: 8 unique messages (exceeds MaxChunkSize) → own chunk, not split
	for i := 0; i < 8; i++ {
		msgs = append(msgs, Message{ChannelName: "errors", Text: "error " + string(rune('A'+i))})
	}
	// Small channel: 2 messages → coalesced
	msgs = append(msgs, Message{ChannelName: "general", Text: "hello"})
	msgs = append(msgs, Message{ChannelName: "general", Text: "world"})

	chunks := e.buildChunks(msgs)

	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks (1 large + 1 small), got %d", len(chunks))
	}
	// Large channel stays whole
	if len(chunks[0].messages) != 8 {
		t.Errorf("expected 8 messages in large chunk, got %d", len(chunks[0].messages))
	}
	if len(chunks[1].messages) != 2 {
		t.Errorf("expected 2 messages in small chunk, got %d", len(chunks[1].messages))
	}
}

func TestFindMatchingBracket_Simple(t *testing.T) {
	idx := findMatchingBracket(`[]`, 0)
	if idx != 1 {
		t.Errorf("expected 1, got %d", idx)
	}
}

func TestFindMatchingBracket_WithContent(t *testing.T) {
	idx := findMatchingBracket(`[{"a":1}]`, 0)
	if idx != 8 {
		t.Errorf("expected 8, got %d", idx)
	}
}

func TestFindMatchingBracket_Nested(t *testing.T) {
	idx := findMatchingBracket(`[[1,2],[3,4]]`, 0)
	if idx != 12 {
		t.Errorf("expected 12, got %d", idx)
	}
}

func TestFindMatchingBracket_BracketsInString(t *testing.T) {
	idx := findMatchingBracket(`["[]"]`, 0)
	if idx != 5 {
		t.Errorf("expected 5, got %d", idx)
	}
}

func TestFindMatchingBracket_EscapedQuotes(t *testing.T) {
	idx := findMatchingBracket(`["val\"ue"]`, 0)
	if idx != 10 {
		t.Errorf("expected 10, got %d", idx)
	}
}

func TestFindMatchingBracket_NoMatch(t *testing.T) {
	idx := findMatchingBracket(`[unclosed`, 0)
	if idx != -1 {
		t.Errorf("expected -1, got %d", idx)
	}
}

func TestParseOpportunities_ValidArray(t *testing.T) {
	data := []byte(`[{"summary":"fix bug","category":"bug","confidence":0.96,"estimated_size":"small","message_index":0,"keywords":["err"],"files_hint":["main.go"]}]`)
	opps, err := parseOpportunities(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opps) != 1 {
		t.Fatalf("expected 1 opportunity, got %d", len(opps))
	}
	if opps[0].Summary != "fix bug" {
		t.Errorf("expected 'fix bug', got %q", opps[0].Summary)
	}
	if opps[0].Confidence != 0.96 {
		t.Errorf("expected 0.96, got %f", opps[0].Confidence)
	}
}

func TestParseOpportunities_EmptyArray(t *testing.T) {
	opps, err := parseOpportunities([]byte(`[]`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opps) != 0 {
		t.Errorf("expected 0 opportunities, got %d", len(opps))
	}
}

func TestParseOpportunities_WithCodeFences(t *testing.T) {
	data := []byte("```json\n[{\"summary\":\"fix\",\"category\":\"bug\",\"confidence\":0.9,\"estimated_size\":\"tiny\",\"message_index\":0}]\n```")
	opps, err := parseOpportunities(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opps) != 1 {
		t.Errorf("expected 1 opportunity, got %d", len(opps))
	}
}

func TestParseOpportunities_WithTrailingText(t *testing.T) {
	data := []byte(`[{"summary":"fix","category":"bug","confidence":0.9,"estimated_size":"tiny","message_index":0}]

**Reasoning**: The message describes a clear bug.`)
	opps, err := parseOpportunities(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opps) != 1 {
		t.Errorf("expected 1 opportunity, got %d", len(opps))
	}
}

func TestParseOpportunities_NoArray(t *testing.T) {
	_, err := parseOpportunities([]byte(`no json here`))
	if err == nil {
		t.Error("expected error for no JSON array")
	}
}

func TestParseOpportunities_MalformedJSON(t *testing.T) {
	_, err := parseOpportunities([]byte(`[{broken`))
	if err == nil {
		t.Error("expected error for malformed JSON")
	}
}

func TestPassesGuardrails_AllPass(t *testing.T) {
	cfg := &config.DigestConfig{
		MinConfidence:     0.95,
		AllowedCategories: []string{"bug", "feature"},
		MaxEstSize:        "small",
	}
	e := &Engine{cfg: cfg}
	opp := Opportunity{Confidence: 0.96, Category: "bug", EstSize: "small"}
	if !e.passesGuardrails(opp) {
		t.Error("expected opportunity to pass guardrails")
	}
}

func TestPassesGuardrails_LowConfidence(t *testing.T) {
	cfg := &config.DigestConfig{
		MinConfidence:     0.95,
		AllowedCategories: []string{"bug"},
		MaxEstSize:        "small",
	}
	e := &Engine{cfg: cfg}
	opp := Opportunity{Confidence: 0.80, Category: "bug", EstSize: "small"}
	if e.passesGuardrails(opp) {
		t.Error("expected low confidence to be filtered")
	}
}

func TestPassesGuardrails_WrongCategory(t *testing.T) {
	cfg := &config.DigestConfig{
		MinConfidence:     0.95,
		AllowedCategories: []string{"bug"},
		MaxEstSize:        "small",
	}
	e := &Engine{cfg: cfg}
	opp := Opportunity{Confidence: 0.96, Category: "feature", EstSize: "small"}
	if e.passesGuardrails(opp) {
		t.Error("expected wrong category to be filtered")
	}
}

func TestPassesGuardrails_TooLarge(t *testing.T) {
	cfg := &config.DigestConfig{
		MinConfidence:     0.95,
		AllowedCategories: []string{"bug"},
		MaxEstSize:        "small",
	}
	e := &Engine{cfg: cfg}
	opp := Opportunity{Confidence: 0.96, Category: "bug", EstSize: "medium"}
	if e.passesGuardrails(opp) {
		t.Error("expected medium size to be filtered when max is small")
	}
}

func TestPassesGuardrails_TinyAlwaysPasses(t *testing.T) {
	cfg := &config.DigestConfig{
		MinConfidence:     0.95,
		AllowedCategories: []string{"bug"},
		MaxEstSize:        "small",
	}
	e := &Engine{cfg: cfg}
	opp := Opportunity{Confidence: 0.96, Category: "bug", EstSize: "tiny"}
	if !e.passesGuardrails(opp) {
		t.Error("expected tiny size to pass when max is small")
	}
}

func TestPassesGuardrails_MaxSizeTiny(t *testing.T) {
	cfg := &config.DigestConfig{
		MinConfidence:     0.95,
		AllowedCategories: []string{"bug"},
		MaxEstSize:        "tiny",
	}
	e := &Engine{cfg: cfg}

	if e.passesGuardrails(Opportunity{Confidence: 0.96, Category: "bug", EstSize: "small"}) {
		t.Error("expected small to be filtered when max is tiny")
	}
	if !e.passesGuardrails(Opportunity{Confidence: 0.96, Category: "bug", EstSize: "tiny"}) {
		t.Error("expected tiny to pass when max is tiny")
	}
}

func TestPassesGuardrails_ExactConfidenceThreshold(t *testing.T) {
	cfg := &config.DigestConfig{
		MinConfidence:     0.95,
		AllowedCategories: []string{"bug"},
		MaxEstSize:        "small",
	}
	e := &Engine{cfg: cfg}
	// Exactly at threshold passes (comparison is <, not <=)
	opp := Opportunity{Confidence: 0.95, Category: "bug", EstSize: "small"}
	if !e.passesGuardrails(opp) {
		t.Error("expected exact threshold to pass (comparison is strict less-than)")
	}
	// Just below threshold should fail
	opp2 := Opportunity{Confidence: 0.949, Category: "bug", EstSize: "small"}
	if e.passesGuardrails(opp2) {
		t.Error("expected below-threshold to be filtered")
	}
}

func TestProcessOpportunities_SpawnLimitReturnsFalse(t *testing.T) {
	cfg := &config.DigestConfig{
		MinConfidence:     0.5,
		AllowedCategories: []string{"bug"},
		MaxEstSize:        "small",
		MaxAutoSpawnHour:  1,
		DryRun:            true,
	}
	e := &Engine{
		cfg:   cfg,
		spawn: func(ctx context.Context, task tadpole.Task) error { return nil },
	}

	msgs := []Message{
		{Text: "bug 1", Channel: "C1", ChannelName: "errors", Timestamp: "1"},
		{Text: "bug 2", Channel: "C1", ChannelName: "errors", Timestamp: "2"},
	}
	opps := []Opportunity{
		{Summary: "fix 1", Category: "bug", Confidence: 0.99, EstSize: "small", MessageIdx: 0},
		{Summary: "fix 2", Category: "bug", Confidence: 0.99, EstSize: "small", MessageIdx: 1},
	}

	result := e.processOpportunities(context.Background(), msgs, opps)

	// With MaxAutoSpawnHour=1, the second opportunity should hit the limit
	if result {
		t.Error("expected processOpportunities to return false when spawn limit reached")
	}
	if e.totalSpawns.Load() != 1 {
		t.Errorf("expected 1 spawn, got %d", e.totalSpawns.Load())
	}
}

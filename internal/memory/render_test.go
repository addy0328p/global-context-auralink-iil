package memory

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func testPolicy() Policy {
	return Policy{AsOfDate: "2026-10-02", ActivityDays: 90, ConversationDays: 30, MaxBytes: 6000}
}
func testInput(role string) Input {
	return Input{Scope: Scope{Tenant: "test", Namespace: "historical_dataset", GLID: "9007199254740993", Role: role}, Profile: map[string]any{}, Provenance: map[string]any{}, Revision: 10}
}
func testEvent(id int64, source, thread, rawTime string, payload map[string]any) Event {
	if (source == "buyer.LAST_WA9696_CHAT" || source == "buyer.LAST_WA8181_CHAT") && payload["MESSAGE_SENDER"] == nil {
		payload["MESSAGE_SENDER"] = "USER"
	}
	return Event{ID: id, Source: source, ThreadRef: thread, RawTime: rawTime, TimeQuality: "unresolved", Channel: "app", Payload: payload, Kind: "buyer.requirement_snapshot"}
}

func TestRoleThreadIsolationAndConflictingFacts(t *testing.T) {
	in := testInput("buyer")
	in.Profile = map[string]any{"buyer": map[string]any{"city": "Noida", "input_mobile_verification_status": json.Number("7")}, "seller": map[string]any{"top_category_1": "not-buyer-inventory", "do_not_call_requested": "1.0"}}
	in.Events = []Event{
		testEvent(1, "buyer.LAST_ENQUIRY", "enquiry:a", "2026-10-01 10:00:00", map[string]any{"PRODUCT_NAME": "Steel rods"}),
		testEvent(2, "buyer.LAST_ENQUIRY", "enquiry:b", "2026-10-01 10:00:00", map[string]any{"PRODUCT_NAME": "Cotton shirts"}),
		testEvent(3, "seller.gc_buyer_call_extractions.csv", "", "2026-10-01 10:00:00", map[string]any{"products_discussed": "Cement", "stock_status": "in stock", "prices_quoted": "100"}),
	}
	p := testPolicy()
	p.ThreadRef = "enquiry:a"
	c, err := Build(in, p, 1)
	if err != nil {
		t.Fatal(err)
	}
	if c.ActiveRole != "buyer" || c.ActiveThreadRef != "enquiry:a" || c.PreferredLanguage != "" {
		t.Fatal("role, thread or language was inferred incorrectly")
	}
	if strings.Contains(c.MemoryMD, "not-buyer-inventory") {
		t.Fatal("seller inventory leaked into buyer profile")
	}
	if !strings.Contains(c.MemoryMD, "Do not initiate outbound calls") {
		t.Fatal("identity contact restriction was lost across roles")
	}
	for _, fact := range c.CurrentConstraints {
		if fact.Field != "do_not_call" && fact.Value != "Steel rods" {
			t.Fatal("other thread or AI-extracted price became a current constraint")
		}
	}
	for _, field := range c.MissingFields {
		if field == "product" {
			t.Fatal("explicit product was re-requested")
		}
	}
	// Conflicting values are retained, rather than picking receipt order as truth.
	in.Events = append(in.Events, testEvent(4, "buyer.LAST_ENQUIRY", "enquiry:a", "2026-10-01 09:00:00", map[string]any{"PRODUCT_NAME": "Copper rods"}))
	c, err = Build(in, p, 2)
	if err != nil {
		t.Fatal(err)
	}
	var conflicts int
	for _, f := range c.CurrentConstraints {
		if f.Status == "conflict" {
			conflicts++
		}
	}
	if conflicts != 2 || !strings.Contains(c.NextQuestion, "conflicting") {
		t.Fatal("conflicting historical products were silently resolved")
	}
	if _, err := Build(in, Policy{AsOfDate: p.AsOfDate, ActivityDays: 90, ConversationDays: 30, MaxBytes: 6000, ThreadRef: "enquiry:other-role"}, 2); err == nil {
		t.Fatal("unknown role-bound thread accepted")
	}
}

func TestHistoryWindowsUseSourceDates(t *testing.T) {
	in := testInput("buyer")
	in.Events = []Event{
		testEvent(1, "buyer.LAST_WA9696_CHAT", "", "20260930120000", map[string]any{"CHAT_CONTENT": "RECENT_TEXT"}),
		testEvent(2, "buyer.LAST_WA9696_CHAT", "", "2026-07-01 10:00:00", map[string]any{"CHAT_CONTENT": "OLD_TEXT"}),
		testEvent(3, "buyer.LAST_WA9696_CHAT", "", "2026-10-03 10:00:00", map[string]any{"CHAT_CONTENT": "FUTURE_TEXT"}),
		testEvent(4, "buyer.LAST_WA9696_CHAT", "", "", map[string]any{"CHAT_CONTENT": "UNDATED_TEXT"}),
		testEvent(5, "seller.gc_whatsapp_messages.csv", "", "2026-10-01 10:00:00", map[string]any{"text": "DELIVERY_IS_NOT_TEXT"}),
	}
	c, err := Build(in, testPolicy(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.MemoryMD, "OLD") || strings.Contains(c.MemoryMD, "FUTURE") || strings.Contains(c.MemoryMD, "DELIVERY\\_IS") {
		t.Fatal("old, future or delivery-only data became conversation text")
	}
	if len(c.Excerpts) != 2 || c.UntimedExcerpts != 1 {
		t.Fatal("qualified undated evidence was dropped or labelled recent")
	}
	var matched bool
	for _, coverage := range c.SourceCoverage {
		if coverage.Source == "buyer.LAST_WA9696_CHAT" {
			matched = coverage.InWindow == 1 && coverage.OutsideWindow == 1 && coverage.Future == 1 && coverage.Undated == 1
		}
	}
	if !matched {
		t.Fatal("source date coverage is inconsistent")
	}
	if SourceDay("20260930120000") != "2026-09-30" || SourceDay("not-a-date") != "" {
		t.Fatal("compact buyer timestamp or invalid date interpreted incorrectly")
	}
}

func TestCompactMemoryKeepsWholeEvidenceAndContactRestriction(t *testing.T) {
	in := testInput("seller")
	in.Profile = map[string]any{"seller": map[string]any{"do_not_call_requested": "1"}}
	in.Events = append(in.Events, testEvent(1, "seller.gc_whatsapp_chatbot_conversations.csv", "", "2026-10-01 10:00:00", map[string]any{"user_message": strings.Repeat("long conditional message ", 50) + "do not buy"}))
	for i := int64(2); i < 40; i++ {
		in.Events = append(in.Events, testEvent(i, "seller.gc_whatsapp_chatbot_conversations.csv", "", "2026-10-01 10:00:00", map[string]any{"user_message": "हमें माल नहीं चाहिए। <script>bad()</script> Contact buyer@example.org or 9876543210 https://example.org/private"}))
	}
	p := testPolicy()
	p.MaxBytes = 3000
	c, err := Build(in, p, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.MemoryMD) > 3000 || !utf8.ValidString(c.MemoryMD) || c.OmittedItems == 0 {
		t.Fatal("compactness or UTF-8 integrity failed")
	}
	if !strings.Contains(c.MemoryMD, "Do not initiate outbound calls") || strings.Contains(c.MemoryMD, "long conditional") {
		t.Fatal("restriction was dropped or a long statement was cut")
	}
	if strings.Contains(c.MemoryMD, "<script>") || strings.Contains(c.MemoryMD, "buyer@example.org") || strings.Contains(c.MemoryMD, "9876543210") || strings.Contains(c.MemoryMD, "https://example.org") {
		t.Fatal("unsafe markup or private contact data was exported")
	}
	d, err := Build(in, p, 1)
	if err != nil {
		t.Fatal(err)
	}
	if c.MemoryMD != d.MemoryMD || hash([]byte(c.MemoryMD)) != hash([]byte(d.MemoryMD)) {
		t.Fatal("memory compilation is nondeterministic")
	}
}

func TestColdStartDoesNotInventRequirements(t *testing.T) {
	c, err := Build(testInput("buyer"), testPolicy(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !c.ColdStart || len(c.Requirements) != 0 || len(c.CurrentConstraints) != 0 || len(c.Excerpts) != 0 || c.ActiveThreadRef != "" {
		t.Fatal("cold start invented personal history")
	}
	var envelope map[string]any
	b, _ := marshal(c)
	if err := decode(b, &envelope); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"memory_md", "memory_version", "active_role", "active_thread_ref", "missing_fields", "next_question", "current_constraints", "preferred_language"} {
		if _, ok := envelope[key]; !ok {
			t.Fatalf("teammate contract field missing: %s", key)
		}
	}
}

func TestProfileObservationOrderingAndResolvedTimestampFallback(t *testing.T) {
	in := testInput("buyer")
	in.Events = []Event{
		{ID: 1, Source: "buyer.api_response", Kind: "profile.snapshot", RawTime: "Sep 30, 2026 @ 12:00:00", TimeQuality: "unresolved", Payload: map[string]any{"engagement": map[string]any{"enq_last_30_days": json.Number("1")}}},
		{ID: 2, Source: "buyer.api_response", Kind: "profile.snapshot", RawTime: "Oct 1, 2026 @ 12:00:00", TimeQuality: "unresolved", Payload: map[string]any{"engagement": map[string]any{"enq_last_30_days": json.Number("2")}}},
	}
	resolved := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	in.Events = append(in.Events, Event{ID: 3, Source: "buyer.LAST_WA9696_CHAT", TimeQuality: "verified", OccurredAt: &resolved, Payload: map[string]any{"CHAT_CONTENT": "Resolved evidence", "MESSAGE_SENDER": "USER"}})
	c, err := Build(in, testPolicy(), 1)
	if err != nil {
		t.Fatal(err)
	}
	var correct bool
	for _, f := range c.ProfileFacts {
		if f.Field == "enq_last_30_days" {
			correct = f.Value == "2"
		}
	}
	if !correct {
		t.Fatal("calendar month names were sorted lexically instead of by source date")
	}
	if len(c.Excerpts) != 1 || c.UntimedExcerpts != 0 || c.Excerpts[0].Evidence.RawTime != "" || c.Excerpts[0].Evidence.OccurredAt == nil {
		t.Fatal("resolved timestamp was lost or fabricated as original source text")
	}
}

func TestStructuredWhatsAppReplyIsReadableAndNotAnAutomatedPrompt(t *testing.T) {
	in := testInput("buyer")
	in.Events = []Event{
		testEvent(1, "buyer.LAST_WA9696_CHAT", "", "2026-10-01 10:00:00", map[string]any{"MESSAGE_SENDER": "USER", "CHAT_CONTENT": `{"context":null,"message_content":{"text":"I do not need it"}}`}),
		testEvent(2, "buyer.LAST_WA9696_CHAT", "", "2026-10-01 10:00:00", map[string]any{"MESSAGE_SENDER": "API", "CHAT_CONTENT": `{"message_content":{"interactive":{"body":{"text":"AUTOMATED_PROMPT"}}}}`}),
	}
	c, err := Build(in, testPolicy(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Excerpts) != 1 || c.Excerpts[0].Text != "I do not need it" || strings.Contains(c.MemoryMD, "AUTOMATED") || strings.Contains(c.MemoryMD, "message\\_content") {
		t.Fatal("structured reply was dumped as JSON or confused with a provider prompt")
	}
	if escaped(`"quote" <script>`) != "\"quote\" &lt;script&gt;" {
		t.Fatal("HTML escaping damaged source text")
	}
}

func TestChannelEvidenceSurvivesABusyEnquiryStream(t *testing.T) {
	in := testInput("seller")
	for id := int64(1); id <= 15; id++ {
		e := testEvent(id, "seller.gc_enquiries_received.csv", "enquiry:many", "2026-10-02 10:00:00", map[string]any{"product_name": "Tape", "message": "Recent enquiry"})
		in.Events = append(in.Events, e)
	}
	voice := testEvent(16, "seller.gc_bot_call_turns.csv", "vani-call:one", "2026-09-10 10:00:00", map[string]any{"speaker": "seller", "text": "Please call after lunch"})
	voice.Channel = "voice"
	wa := testEvent(17, "seller.gc_whatsapp_chatbot_conversations.csv", "", "2026-09-10 10:00:00", map[string]any{"user_message": "I want help with my listing"})
	wa.Channel = "whatsapp"
	in.Events = append(in.Events, voice, wa)
	c, err := Build(in, testPolicy(), 1)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, x := range c.Excerpts {
		seen[x.Channel] = true
	}
	if !seen["voice"] || !seen["whatsapp"] || !seen["app"] {
		t.Fatal("one busy channel displaced all evidence from other channels")
	}
}

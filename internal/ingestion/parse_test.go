package ingestion

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func csvBytes(t *testing.T, rows [][]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	if err := w.WriteAll(rows); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestBuyerImportPreservesIdentityAndRejectsBrokenRecords(t *testing.T) {
	const glid = "9007199254740993"
	data := map[string]any{
		"glid":       json.Number(glid),
		"ActualData": map[string]any{"glid": json.Number(glid)},
		"kycdetails": map[string]any{"glid": json.Number(glid), "city": "Noida", "email1": "personal@example.org", "address": "private home", "input_mobile_verification_status": 7},
		"BLdetails":  map[string]any{"glid": json.Number(glid), "active_bl_details": []any{map[string]any{"ETO_OFR_ID": json.Number(glid), "ETO_OFR_TITILE": "steel rods"}}},
		"activitydetails": map[string]any{"glid": json.Number(glid),
			"BUYER_ACTIVITY":   []any{map[string]any{"ACTIVITY_ID": json.Number(glid), "ACTIVITY_TYPE": 99, "KEYWORD": "steel"}},
			"LAST_ENQUIRY":     map[string]any{"ENQUIRY_ID": "42", "DESCRIPTION": "Call 9876543210 or buyer@example.org https://example.org/file"},
			"LAST_LEAD":        map[string]any{"LEAD_ID": "43", "TITLE": "steel", "DATE_R": "2026-10-08 10:00:00", "IMAGE_PATH1": "https://example.org/photo"},
			"ISQ_DATA":         map[string]any{"ISQ": []any{}},
			"LAST_WA9696_CHAT": []any{map[string]any{"CHAT_CONTENT": "200 kilograms", "ENTRY_DATE": "2026-10-08 10:00:00", "MESSAGE_SENDER": "USER"}},
		},
	}
	response := func(d map[string]any) string {
		return string(encode(map[string]any{"API_RESPONSE_JSON": string(encode(map[string]any{"code": 200, "data": d}))}))
	}
	good := response(data)
	bad := map[string]any{"glid": json.Number(glid), "ActualData": map[string]any{"glid": json.Number(glid)}, "kycdetails": map[string]any{"glid": json.Number("123")}}
	rows := [][]string{
		{"@timestamp", "GLUSR_ID", "RESPONSE", "_id", ""},
		{"2026-10-09T10:00:00Z", glid, good, "one", ""},
		{"2026-10-09T11:00:00Z", "42", good, "two", ""},
		{"", glid, response(bad), "three", ""},
		{"", glid, good, "four", "spilled"},
		{"", glid, "{broken", "five", ""},
		{"", glid, `{"code":200}`, "six", ""},
	}
	filename := filepath.Join(t.TempDir(), "buyer.csv")
	if err := os.WriteFile(filename, csvBytes(t, rows), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := parseBuyer(filename, "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Report.Records != 6 || f.Report.Accepted != 2 || f.Report.Quarantined != 4 {
		t.Fatalf("unexpected buyer audit: %+v", f.Report)
	}
	if f.Report.Events != 12 {
		t.Fatalf("empty sections fabricated events: got %d", f.Report.Events)
	}
	if f.Report.Issues["buyer_log_identity_differs_from_subject"] != 1 {
		t.Fatal("log identity was mistaken for response subject")
	}
	for _, e := range f.Events {
		if e.GLID != glid || e.Role != "buyer" {
			t.Fatal("identity or role was changed")
		}
		for _, secret := range []string{"personal@example.org", "9876543210", "buyer@example.org", "https://example.org", "private home"} {
			if strings.Contains(string(e.Payload), secret) {
				t.Fatal("unnecessary personal data leaked into memory input")
			}
		}
		if e.Source == "buyer.LAST_ENQUIRY" && e.OccurredAt != nil {
			t.Fatal("snapshot observation time was substituted for missing enquiry time")
		}
		if e.Source == "buyer.activity" && !strings.Contains(string(e.Payload), glid) {
			t.Fatal("numeric identifier lost precision")
		}
	}
	if !strings.Contains(string(f.Profiles[0].Payload), `"input_mobile_verification_status":7`) {
		t.Fatal("source status enum was reinterpreted")
	}
}

func TestSellerRolesAndCallEvidence(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "seller.zip")
	f, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	fixtures := map[string][][]string{
		"gc_seller_profile.csv":         {{"fk_glusr_usr_id", "company_name"}, {"101", "Steel Works"}},
		"gc_buyer_call_extractions.csv": {{"glid", "file_id", "call_date", "seller_role_on_call"}, {"101", "f1", "2026-10-09 10:00:00", "BUYER"}, {"101", "f2", "2026-10-09 10:00:00", "SELLER"}, {"101", "f3", "", "UNKNOWN"}},
		"gc_bot_calls.csv":              {{"fk_glusr_usr_id", "attempt_id", "call_start_time"}, {"101", "a", "2026-10-09T10:00:00Z"}, {"101", "b", "2026-10-09T10:00:00Z"}},
		"gc_bot_call_turns.csv":         {{"fk_glusr_usr_id", "attempt_id", "turn_no", "speaker", "start_sec", "end_sec", "text"}, {"101", "a", "1", "seller", "0", "1", "yes"}, {"101", "a", "1", "seller", "0", "1", "no"}, {"101", "a", "2", "bot", "1", "2", "ambiguous parent"}, {"101", "b", "1", "seller", "1.5", "2.5", "valid"}, {"101", "orphan", "1", "seller", "0", "1", "no parent"}, {"101", "b", "2", "bot", "3", "2", "invalid duration"}},
	}
	for _, m := range sellerMappings {
		rows, ok := fixtures[m.Name]
		if !ok {
			header := []string{}
			seen := map[string]bool{}
			for _, h := range []string{m.GLID, m.ID, m.Time} {
				if h != "" && !seen[h] {
					header = append(header, h)
					seen[h] = true
				}
			}
			rows = [][]string{header}
		}
		w, err := z.Create(m.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write(csvBytes(t, rows)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := parseSellers(filename, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		switch file.Report.Name {
		case "gc_buyer_call_extractions.csv":
			if len(file.Events) != 2 || file.Events[0].Role != "buyer" || file.Events[1].Role != "seller" {
				t.Fatal("seller's buying activity was attributed to the seller role")
			}
		case "gc_bot_call_turns.csv":
			if len(file.Events) != 1 || file.Report.Quarantined != 5 {
				t.Fatalf("invalid call evidence accepted: %+v", file.Report)
			}
			want := time.Date(2026, 10, 9, 10, 0, 1, 500000000, time.UTC)
			if file.Events[0].OccurredAt == nil || !file.Events[0].OccurredAt.Equal(want) {
				t.Fatal("call turn offset was not preserved")
			}
		}
	}
}

func TestSourceTimeRequiresAnExplicitTimezone(t *testing.T) {
	if value, quality := timeInfo("2026-10-09 10:00:00", ""); value != nil || quality != "unresolved" {
		t.Fatal("timezone was silently assumed")
	}
	value, quality := timeInfo("2026-10-09 10:00:00", "Asia/Kolkata")
	if value == nil || quality != "assumed" || value.UTC().Hour() != 4 || value.UTC().Minute() != 30 {
		t.Fatal("explicit timezone was not applied")
	}
}

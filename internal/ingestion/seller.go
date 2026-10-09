package ingestion

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"path"
	"strconv"
	"strings"
	"time"
)

type sellerMapping struct{ Name, GLID, ID, Time, Kind, Channel string }

var sellerMappings = []sellerMapping{
	{"gc_seller_profile.csv", "fk_glusr_usr_id", "fk_glusr_usr_id", "", "profile.snapshot", "import"},
	{"gc_enquiries_received.csv", "seller_glid", "query_id", "enquiry_date", "enquiry.created", "app"},
	{"gc_enquiry_messages.csv", "seller_glid", "reply_id", "reply_date", "enquiry.message", "app"},
	{"gc_buyer_calls_received.csv", "seller_glid", "call_id", "call_datetime", "call.metadata", "voice"},
	{"gc_buyleads_bought.csv", "seller_glid", "purchase_id", "purchase_date", "buylead.purchased", "app"},
	{"gc_whatsapp_messages.csv", "glid", "message_id", "entry_date", "message.status_observed", "whatsapp"},
	{"gc_whatsapp_chatbot_conversations.csv", "glid", "chat_id", "entry_date", "conversation.paired_turn", "whatsapp"},
	{"gc_buyer_call_extractions.csv", "glid", "file_id", "call_date", "call.extracted_claims", "voice"},
	{"gc_bot_calls.csv", "fk_glusr_usr_id", "attempt_id", "call_start_time", "conversation.completed", "voice"},
	{"gc_bot_call_turns.csv", "fk_glusr_usr_id", "attempt_id", "", "conversation.turn", "voice"},
	{"gc_executive_calls.csv", "fk_glusr_usr_id", "click_to_call_id", "call_start_time", "call.metadata", "voice"},
}

func readCSV(b []byte) ([]map[string]string, error) {
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf})))
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("could not read CSV header")
	}
	seen := map[string]bool{}
	for _, h := range header {
		if h == "" || seen[h] {
			return nil, fmt.Errorf("invalid or duplicate seller header")
		}
		seen[h] = true
	}
	var rows []map[string]string
	for {
		values, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid seller CSV structure at record %d", len(rows)+1)
		}
		row := map[string]string{}
		for i, h := range header {
			row[h] = values[i]
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func parseSellers(filename, zone string) ([]*File, error) {
	z, err := zip.OpenReader(filename)
	if err != nil {
		return nil, fmt.Errorf("could not open seller ZIP")
	}
	defer z.Close()
	entries := map[string]*zip.File{}
	var total uint64
	for _, entry := range z.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		if strings.Contains(entry.Name, "\\") || path.Clean(entry.Name) != entry.Name || strings.HasPrefix(entry.Name, "/") || strings.Contains(entry.Name, "/") {
			return nil, fmt.Errorf("seller ZIP contains unsupported paths")
		}
		if _, exists := entries[entry.Name]; exists {
			return nil, fmt.Errorf("seller ZIP contains duplicate filenames")
		}
		total += entry.UncompressedSize64
		if entry.UncompressedSize64 > 64<<20 || total > 256<<20 {
			return nil, fmt.Errorf("seller ZIP exceeds import size limit")
		}
		entries[entry.Name] = entry
	}
	load := func(name string) ([]byte, []map[string]string, error) {
		entry := entries[name]
		if entry == nil {
			return nil, nil, fmt.Errorf("seller ZIP is missing %s", name)
		}
		r, err := entry.Open()
		if err != nil {
			return nil, nil, fmt.Errorf("could not read %s", name)
		}
		b, err := io.ReadAll(io.LimitReader(r, 64<<20+1))
		r.Close()
		if err != nil || len(b) > 64<<20 {
			return nil, nil, fmt.Errorf("could not read bounded %s", name)
		}
		rows, err := readCSV(b)
		return b, rows, err
	}
	_, profiles, err := load("gc_seller_profile.csv")
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, r := range profiles {
		if digitID.MatchString(r["fk_glusr_usr_id"]) {
			known[r["fk_glusr_usr_id"]] = true
		}
	}
	_, enquiries, err := load("gc_enquiries_received.csv")
	if err != nil {
		return nil, err
	}
	queries := map[string]map[string]string{}
	for _, r := range enquiries {
		queries[r["query_id"]] = r
	}
	_, calls, err := load("gc_bot_calls.csv")
	if err != nil {
		return nil, err
	}
	attempts := map[string]map[string]string{}
	for _, r := range calls {
		attempts[r["attempt_id"]] = r
	}
	_, turns, err := load("gc_bot_call_turns.csv")
	if err != nil {
		return nil, err
	}
	variants := map[string]map[string]bool{}
	for _, r := range turns {
		key := r["attempt_id"] + ":" + r["turn_no"]
		if variants[key] == nil {
			variants[key] = map[string]bool{}
		}
		variants[key][digest(encode(r))] = true
	}
	ambiguousAttempts := map[string]bool{}
	for _, r := range turns {
		if len(variants[r["attempt_id"]+":"+r["turn_no"]]) > 1 {
			ambiguousAttempts[r["attempt_id"]] = true
		}
	}
	var files []*File
	for _, m := range sellerMappings {
		b, rows, err := load(m.Name)
		if err != nil {
			return nil, err
		}
		for _, col := range []string{m.GLID, m.ID, m.Time} {
			if col != "" && len(rows) > 0 {
				if _, ok := rows[0][col]; !ok {
					return nil, fmt.Errorf("missing required field %s in %s", col, m.Name)
				}
			}
		}
		f := newFile(m.Name, digest(b))
		for i, r := range rows {
			glid, role, id, rawTime := r[m.GLID], "seller", r[m.ID], r[m.Time]
			reason, thread := "", ""
			if !digitID.MatchString(glid) || !known[glid] {
				reason = "invalid_or_unknown_seller_glid"
			}
			if id == "" {
				reason = "missing_source_identifier"
			}
			if m.Name == "gc_enquiries_received.csv" {
				thread = "enquiry:" + r["query_id"]
			}
			if m.Name == "gc_enquiry_messages.csv" {
				thread = "enquiry:" + r["query_id"]
				parent := queries[r["query_id"]]
				if parent == nil || parent["seller_glid"] != glid {
					reason = "missing_or_mismatched_enquiry"
				} else {
					sender, receiver := parent["buyer_glid"], glid
					if r["message_from"] == "seller" {
						sender, receiver = receiver, sender
					} else if r["message_from"] != "buyer" {
						reason = "unknown_enquiry_speaker"
					}
					if sender != r["sender_glid"] || receiver != r["receiver_glid"] {
						reason = "enquiry_participant_mismatch"
					}
				}
			}
			if m.Name == "gc_buyer_call_extractions.csv" {
				switch r["seller_role_on_call"] {
				case "BUYER":
					role = "buyer"
				case "SELLER":
					role = "seller"
				default:
					reason = "unknown_call_role"
				}
			}
			if m.Name == "gc_bot_call_turns.csv" {
				parent := attempts[r["attempt_id"]]
				key := r["attempt_id"] + ":" + r["turn_no"]
				id = key + ":" + r["speaker"] + ":" + r["start_sec"]
				if parent == nil || parent["fk_glusr_usr_id"] != glid {
					reason = "orphan_or_mismatched_call_turn"
				} else {
					rawTime = parent["call_start_time"]
				}
				if ambiguousAttempts[r["attempt_id"]] {
					reason = "ambiguous_call_turn_variants"
				}
				if r["speaker"] != "bot" && r["speaker"] != "seller" {
					reason = "unknown_call_speaker"
				}
				start, e1 := strconv.ParseFloat(r["start_sec"], 64)
				end, e2 := strconv.ParseFloat(r["end_sec"], 64)
				if e1 != nil || e2 != nil || math.IsNaN(start) || math.IsNaN(end) || math.IsInf(start, 0) || math.IsInf(end, 0) || start < 0 || end < start {
					reason = "invalid_turn_duration"
				}
				thread = "vani-call:" + r["attempt_id"]
			}
			for _, field := range []string{"duration_sec", "talk_sec", "file_duration_sec", "lead_call_duration", "call_duration", "call_duration_customer", "ring_duration"} {
				if v := r[field]; v != "" {
					n, err := strconv.ParseFloat(v, 64)
					if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
						reason = "invalid_call_duration"
					}
				}
			}
			if m.Name == "gc_bot_calls.csv" {
				thread = "vani-call:" + r["attempt_id"]
			}
			// A WhatsApp session groups messages; it is not proof of a single product requirement.
			if m.Name == "gc_whatsapp_chatbot_conversations.csv" && r["session_id"] == "" {
				f.Report.Issues["missing_session_id"]++
			}
			safe := map[string]any{}
			for k, v := range r {
				if k == "call_recording_url" || k == "media_url" || k == "fk_employeeid" || k == "seller_locality" || k == "seller_pincode" || k == "buyer_designation" {
					continue
				}
				if v == "" {
					safe[k] = nil
					continue
				}
				if strings.HasSuffix(k, "glid") || k == "fk_glusr_usr_id" || strings.HasSuffix(k, "_id") {
					safe[k] = v
					continue
				}
				var structured any
				if (k == "user_message" || k == "bot_reply") && decode([]byte(v), &structured) == nil {
					safe[k] = sanitize(structured)
				} else {
					safe[k] = redact(v)
				}
			}
			safe["source_role"] = role
			if m.Name == "gc_buyer_call_extractions.csv" {
				safe["evidence_quality"] = "source_ai_extraction"
			}
			f.row(i+1, r, safe, reason)
			if reason != "" {
				continue
			}
			if m.Name == "gc_seller_profile.csv" {
				f.Profiles = append(f.Profiles, Profile{GLID: glid, Role: "seller", Payload: encode(safe), Provenance: encode(map[string]any{"source": m.Name, "source_sha256": f.Report.SHA256, "source_record": i + 1, "historical": true, "as_of_unknown": true})})
			}
			f.event(glid, role, "seller."+m.Name, id, m.Kind, m.Channel, thread, rawTime, zone, safe)
			if m.Name == "gc_bot_call_turns.csv" {
				e := &f.Events[len(f.Events)-1]
				if e.OccurredAt != nil {
					start, _ := strconv.ParseFloat(r["start_sec"], 64)
					t := e.OccurredAt.Add(time.Duration(start * float64(time.Second)))
					e.OccurredAt = &t
				}
			}
		}
		files = append(files, f)
	}
	return files, nil
}

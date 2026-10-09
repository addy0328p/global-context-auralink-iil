package ingestion

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata"
)

var digitID = regexp.MustCompile(`^[0-9]+$`)
var email = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)
var phone = regexp.MustCompile(`\b(?:\+?91[ -]?)?[6-9][0-9]{9}\b`)
var url = regexp.MustCompile(`(?i)https?://[^\s"<>]+`)

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func encode(v any) []byte    { b, _ := json.Marshal(v); return b }
func decode(b []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("extra JSON content")
	}
	return nil
}
func text(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return string(x)
	}
	return ""
}
func object(v any) map[string]any { x, _ := v.(map[string]any); return x }
func list(v any) []any            { x, _ := v.([]any); return x }
func redact(s string) string {
	s = email.ReplaceAllString(s, "[email redacted]")
	s = phone.ReplaceAllString(s, "[phone redacted]")
	return url.ReplaceAllString(s, "[link omitted]")
}
func sanitize(v any) any {
	switch x := v.(type) {
	case string:
		return redact(x)
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = sanitize(v)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			lower := strings.ToLower(k)
			if strings.Contains(lower, "url") || strings.Contains(lower, "image_path") || lower == "email1" || lower == "address" || strings.HasPrefix(lower, "account_manager") {
				continue
			}
			if strings.HasSuffix(lower, "_id") || strings.HasSuffix(lower, "glid") {
				out[k] = v
				continue
			}
			out[k] = sanitize(v)
		}
		return out
	default:
		return v
	}
}
func pick(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = sanitize(v)
		}
	}
	return out
}
func timeInfo(raw, zone string) (*time.Time, string) {
	if raw == "" {
		return nil, "unresolved"
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return &t, "verified"
	}
	if zone == "" {
		return nil, "unresolved"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, "unresolved"
	}
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", "Jan 2, 2006 @ 15:04:05.999999999", "2006-01-02", "02-Jan-2006"} {
		if t, err := time.ParseInLocation(layout, raw, loc); err == nil {
			return &t, "assumed"
		}
	}
	return nil, "unresolved"
}

// This key only orders observations within one export. It does not assert a
// timezone or convert a naive source time into an event timestamp.
func sourceSortKey(raw string) string {
	for _, layout := range []string{time.RFC3339Nano, "Jan 2, 2006 @ 15:04:05.999999999", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.Format("2006-01-02T15:04:05.999999999")
		}
	}
	return ""
}
func newFile(name, hash string) *File {
	return &File{Report: FileReport{MappingVersion: MappingVersion, Name: name, SHA256: hash, Issues: map[string]int{}}}
}
func (f *File) row(number int, raw any, safe any, reason string) {
	f.Report.Records++
	if reason == "" {
		f.Report.Accepted++
	} else {
		f.Report.Quarantined++
		f.Report.Issues[reason]++
	}
	f.Rows = append(f.Rows, Row{Number: number, Hash: digest(encode(raw)), Payload: encode(safe), Reason: reason})
}
func (f *File) event(glid, role, source, id, kind, channel, thread, rawTime, zone string, payload any) {
	b := encode(payload)
	occurred, quality := timeInfo(rawTime, zone)
	hash := digest(encode(map[string]any{"glid": glid, "role": role, "thread": thread, "kind": kind, "channel": channel, "raw_time": rawTime, "payload": payload}))
	f.Events = append(f.Events, Event{GLID: glid, Role: role, Source: source, EntityID: id, Revision: hash, Kind: kind, Channel: channel, Thread: thread, Payload: b, Hash: hash, RawTime: rawTime, OccurredAt: occurred, TimeQuality: quality})
	f.Report.Events++
}

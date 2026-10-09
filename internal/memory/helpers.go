package memory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var digits = regexp.MustCompile(`^[0-9]+$`)
var namespacePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var contactEmail = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)
var contactPhone = regexp.MustCompile(`\b(?:\+?91[ -]?)?[6-9][0-9]{9}\b`)
var linkPattern = regexp.MustCompile(`(?i)https?://[^\s"<>]+`)

func fmtID(id int64) string { return strconv.FormatInt(id, 10) }

func sourceFlagOne(v any) bool {
	value, err := strconv.ParseFloat(str(v), 64)
	return err == nil && value == 1
}

func hash(b []byte) string          { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func marshal(v any) ([]byte, error) { return json.Marshal(v) }
func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(v)
}
func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return string(x)
	}
	return ""
}
func obj(v any) map[string]any { x, _ := v.(map[string]any); return x }
func plain(v any) string {
	if s, ok := v.(string); ok && (strings.HasPrefix(strings.TrimSpace(s), "{") || strings.HasPrefix(strings.TrimSpace(s), "[")) {
		var structured any
		if decode([]byte(s), &structured) == nil {
			v = structured
		}
	}
	s := str(v)
	if m := obj(v); m != nil {
		if content := obj(m["message_content"]); content != nil {
			m = content
		}
		if interactive := obj(m["interactive"]); interactive != nil {
			if body := obj(interactive["body"]); body != nil {
				m = body
			}
		}
		for _, key := range []string{"text_query", "text", "body", "message"} {
			if s = str(m[key]); s != "" {
				break
			}
		}
		if s == "" {
			s = str(m["title"])
		}
	}
	return cleanText(s)
}

func cleanText(s string) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	s = contactEmail.ReplaceAllString(s, "[email redacted]")
	s = contactPhone.ReplaceAllString(s, "[phone redacted]")
	return linkPattern.ReplaceAllString(s, "[link omitted]")
}
func escaped(s string) string {
	s = strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "#", "\\#", "`", "\\`", "|", "\\|", "!", "\\!").Replace(s)
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
func validateScope(s Scope) error {
	if s.Tenant == "" || !namespacePattern.MatchString(s.Namespace) || !digits.MatchString(s.GLID) || (s.Role != "buyer" && s.Role != "seller") {
		return errors.New("invalid memory scope")
	}
	return nil
}
func (p Policy) Validate() error {
	if _, err := time.Parse("2006-01-02", p.AsOfDate); err != nil {
		return errors.New("memory reference date must be YYYY-MM-DD")
	}
	if p.ActivityDays < 1 || p.ActivityDays > 365 || p.ConversationDays < 1 || p.ConversationDays > p.ActivityDays {
		return errors.New("invalid memory history windows")
	}
	if p.MaxBytes < 3000 || p.MaxBytes > 16000 {
		return errors.New("memory byte budget must be between 3000 and 16000")
	}
	if len(p.ThreadRef) > 256 {
		return errors.New("thread reference is too long")
	}
	return nil
}
func policyHash(p Policy) string {
	b, _ := marshal(struct {
		Version string
		Policy  Policy
	}{RendererVersion, p})
	return hash(b)
}

// SourceDay uses the source's calendar date, without asserting a timezone or
// treating received_at as activity time. Compact buyer dates are in the export.
func SourceDay(raw string) string {
	if t, ok := sourceCalendarTime(raw); ok {
		return t.Format("2006-01-02")
	}
	return ""
}

func sourceCalendarTime(raw string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", "20060102150405", "Jan 2, 2006 @ 15:04:05.999999999", "2006-01-02", "02-Jan-2006"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func sourceCalendarKey(raw string) string {
	if t, ok := sourceCalendarTime(raw); ok {
		return t.Format("2006-01-02T15:04:05.999999999")
	}
	return ""
}
func eventDay(e Event) string {
	if day := SourceDay(e.RawTime); day != "" {
		return day
	}
	if e.RawTime == "" && e.OccurredAt != nil && (e.TimeQuality == "verified" || e.TimeQuality == "synthetic") {
		return e.OccurredAt.UTC().Format("2006-01-02")
	}
	return ""
}
func evidence(e Event, field string) Evidence {
	return Evidence{EventID: fmtID(e.ID), Source: e.Source, EntityID: e.EntityID, Field: field, RawTime: e.RawTime, TimeQuality: e.TimeQuality, OccurredAt: e.OccurredAt}
}
func profileEvidence(m map[string]any) Evidence {
	row, _ := m["source_record"].(json.Number)
	return Evidence{Source: str(m["source"]), SourceHash: str(m["source_sha256"]), SourceRow: row, RawTime: str(m["source_time_raw"]), TimeQuality: "unresolved"}
}

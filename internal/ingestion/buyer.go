package ingestion

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"
)

func Parse(opts Options) (*Dataset, error) {
	if opts.SourceTimezone != "" {
		if _, q := timeInfo("2026-10-09 09:00:00", opts.SourceTimezone); q != "assumed" {
			return nil, fmt.Errorf("invalid SOURCE_TIMEZONE; use a valid IANA timezone or leave it blank")
		}
	}
	files, err := parseSellers(opts.SellerZIP, opts.SourceTimezone)
	if err != nil {
		return nil, err
	}
	buyer, err := parseBuyer(opts.BuyerCSV, opts.SourceTimezone)
	if err != nil {
		return nil, err
	}
	return &Dataset{Files: append(files, buyer)}, nil
}

func hasContent(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(x) != ""
	case map[string]any:
		for _, v := range x {
			if hasContent(v) {
				return true
			}
		}
		return false
	case []any:
		for _, v := range x {
			if hasContent(v) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func parseBuyer(filename, zone string) (*File, error) {
	b, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("could not read buyer CSV")
	}
	if len(b) > 64<<20 {
		return nil, fmt.Errorf("buyer CSV exceeds import size limit")
	}
	f := newFile("Global_Context_Problem_Statement_Buyer Side.csv", digest(b))
	f.Report.MappingVersion = MappingVersion + "-buyer2"
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf})))
	r.LazyQuotes = true
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("could not read buyer CSV header")
	}
	indices := map[string]int{}
	for i, h := range header {
		if h != "" {
			if _, ok := indices[h]; ok {
				return nil, fmt.Errorf("duplicate named buyer column")
			}
			indices[h] = i
		}
	}
	for _, required := range []string{"RESPONSE", "GLUSR_ID", "@timestamp"} {
		if _, ok := indices[required]; !ok {
			return nil, fmt.Errorf("missing required buyer column %s", required)
		}
	}
	for number := 1; ; number++ {
		values, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("buyer CSV reader failed at record %d", number)
		}
		get := func(k string) string {
			i, ok := indices[k]
			if !ok || i >= len(values) {
				return ""
			}
			return values[i]
		}
		reason := ""
		if len(values) != len(header) {
			reason = "buyer_column_count_mismatch"
		}
		for i, h := range header {
			if h == "" && i < len(values) && strings.TrimSpace(values[i]) != "" {
				reason = "buyer_spilled_columns"
				break
			}
		}
		var wrapper map[string]any
		if reason == "" && decode([]byte(get("RESPONSE")), &wrapper) != nil {
			reason = "buyer_invalid_response_json"
		}
		var response map[string]any
		if reason == "" {
			switch x := wrapper["API_RESPONSE_JSON"].(type) {
			case string:
				if decode([]byte(x), &response) != nil {
					reason = "buyer_invalid_nested_json"
				}
			case map[string]any:
				response = x
			default:
				reason = "buyer_missing_nested_response"
			}
		}
		data := object(response["data"])
		if reason == "" && (data == nil || text(response["code"]) != "200") {
			reason = "buyer_response_without_successful_data"
		}
		glid := text(object(data["ActualData"])["glid"])
		if glid == "" {
			glid = text(data["glid"])
		}
		if reason == "" && !digitID.MatchString(glid) {
			reason = "buyer_missing_or_invalid_glid"
		}
		if reason == "" {
			if v := text(data["glid"]); v != "" && v != glid {
				reason = "buyer_section_glid_mismatch"
			}
			for _, section := range []string{"ActualData", "activitydetails", "kycdetails", "connectdetails", "BLdetails", "ticketsdetails", "duplicate_glids_details"} {
				v := text(object(data[section])["glid"])
				if v != "" && v != glid {
					reason = "buyer_section_glid_mismatch"
				}
			}
			if v := get("GLUSR_ID"); digitID.MatchString(v) && v != glid {
				// The export's log identity is different from its response subject.
				// Do not use it as the customer join key.
				f.Report.Issues["buyer_log_identity_differs_from_subject"]++
			}
		}
		if reason != "" {
			// The original remains in the local CSV; don't copy broken responses,
			// which can contain unrestricted personal fields, into logs or staging.
			f.row(number, values, map[string]any{"parse_status": reason}, reason)
			continue
		}
		kyc := object(data["kycdetails"])
		activity := object(data["activitydetails"])
		profile := pick(kyc, "city", "country", "company_name", "company_type", "input_email_verification_status", "input_mobile_verification_status", "address_verification_status", "gst_status", "gst_availability", "registered_since_with_indiamart")
		profile["preferred_cities"] = sanitize(object(data["ActualData"])["preferred_cities"])
		profile["historical"] = true
		profile["verification_enum_mapping"] = "raw_source_values_not_interpreted"
		rawTime := get("@timestamp")
		sourceID := get("_id")
		if sourceID == "" {
			sourceID = digest(encode(values))
		}
		provenance := map[string]any{"source": "buyer.api_response", "source_sha256": f.Report.SHA256, "source_record": number, "historical": true, "source_time_raw": rawTime, "source_sort_key": sourceSortKey(rawTime), "identity_source": "response_payload_glid"}
		f.Profiles = append(f.Profiles, Profile{GLID: glid, Role: "buyer", Payload: encode(profile), Provenance: encode(provenance)})
		snapshot := map[string]any{"glid": glid, "profile": profile, "source_time_raw": rawTime, "historical": true}
		// Only documented useful activity counters are included, retaining their original windows.
		snapshot["engagement"] = pick(activity, "enq_last_30_days", "enq_last_90_days", "pns_received_30_days", "pns_received_90_days", "hl_pns_received_30d", "hl_pns_success_30d")
		f.row(number, values, snapshot, "")
		f.event(glid, "buyer", "buyer.api_response", sourceID, "profile.snapshot", "import", "", rawTime, zone, snapshot)
		for _, item := range list(object(data["BLdetails"])["active_bl_details"]) {
			m := object(item)
			if m == nil {
				f.Report.Issues["buyer_invalid_active_buylead"]++
				continue
			}
			p := pick(m, "ETO_OFR_DISPLAY_ID", "ETO_OFR_ID", "ETO_OFR_TITILE")
			if !hasContent(p) {
				continue
			}
			id := text(m["ETO_OFR_ID"])
			if id == "" {
				id = text(m["ETO_OFR_DISPLAY_ID"])
			}
			if id == "" {
				id = digest(encode(p))
			}
			// The source has a title and identifier, with no reliable creation time.
			f.event(glid, "buyer", "buyer.active_buylead", glid+":"+id, "buyer.requirement_snapshot", "app", "buyer-lead:"+id, "", zone, p)
		}
		for _, item := range list(activity["BUYER_ACTIVITY"]) {
			m := object(item)
			if m == nil {
				f.Report.Issues["buyer_invalid_activity_item"]++
				continue
			}
			p := pick(m, "ACTIVITY_ID", "ACTIVITY_TIME", "ACTIVITY_TYPE", "CATEGORY_ID", "CATEGORY_NAME", "KEYWORD", "PRODUCT_ID", "SELLER_GLUSR_ID")
			id := text(m["ACTIVITY_ID"])
			if id == "" {
				id = digest(encode(p))
			}
			f.event(glid, "buyer", "buyer.activity", glid+":"+id, "buyer.activity", "app", "", text(m["ACTIVITY_TIME"]), zone, p)
		}
		for _, field := range []string{"LAST_ENQUIRY", "LAST_LEAD", "ISQ_DATA"} {
			m := object(activity[field])
			if !hasContent(m) {
				continue
			}
			p := sanitize(m)
			id := text(m["ENQUIRY_ID"])
			if id == "" {
				id = text(m["LEAD_ID"])
			}
			if id == "" {
				id = digest(encode(p))
			}
			thread := ""
			if field == "LAST_ENQUIRY" {
				thread = "enquiry:" + id
			}
			if field == "LAST_LEAD" {
				thread = "buyer-lead:" + id
			}
			// Snapshot observation time is not substituted for missing enquiry time.
			f.event(glid, "buyer", "buyer."+field, glid+":"+id, "buyer.requirement_snapshot", "app", thread, text(m["DATE_R"]), zone, p)
		}
		for _, field := range []string{"LAST_WA8181_CHAT", "LAST_WA9696_CHAT"} {
			for _, item := range list(activity[field]) {
				m := object(item)
				if m == nil {
					continue
				}
				p := pick(m, "CHAT_CONTENT", "ENTRY_DATE", "MESSAGE_SENDER")
				f.event(glid, "buyer", "buyer."+field, glid+":"+digest(encode(p)), "conversation.turn", "whatsapp", "", text(m["ENTRY_DATE"]), zone, p)
			}
		}
		connect := object(data["connectdetails"])
		for _, field := range []string{"connect_30D", "connect_7D"} {
			for _, item := range list(connect[field]) {
				m := object(item)
				if m == nil {
					continue
				}
				p := pick(m, "call_answered_date", "call_answered_summary", "call_answered_transcript", "call_sts_update", "meet_sts_update", "meet_sucessful_date")
				if !hasContent(p) {
					continue
				}
				f.event(glid, "buyer", "buyer.connect", glid+":"+digest(encode(p)), "call.metadata", "voice", "", text(m["call_answered_date"]), zone, p)
			}
		}
	}
	return f, nil
}

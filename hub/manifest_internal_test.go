package hub

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateManifest(t *testing.T) {
	valid := map[string]string{
		"minimal": `{"manifestRevision": 1}`,
		"full": `{"manifestRevision": 7,
			"game": "test", "plugin": {"name": "p", "version": "1"},
			"actions": [{"code": "m.heal", "name": "Heal", "context": "player",
				"namespace": "m", "danger": "warning",
				"params": {"type": "object", "required": ["amount"],
					"properties": {"amount": {"type": "integer", "minimum": 1}}}}],
			"contexts": [{"id": "territory", "name": "Territory", "namespace": "m"}],
			"events": [{"id": "m.raid", "name": "Raid", "namespace": "m",
				"payload": {"type": "object"}}]}`,
		"unknownTopLevelField": `{"manifestRevision": 1, "fieldFromALaterDraft": true}`,
		"nullParams":           `{"manifestRevision": 1, "actions": [{"code": "m.x", "params": null}]}`,
	}
	for name, body := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			revision, faults := validateManifest(json.RawMessage(body))
			if len(faults) > 0 {
				t.Fatalf("faults = %v, want none", faults)
			}
			if revision < 1 {
				t.Fatalf("revision = %d, want the published one", revision)
			}
		})
	}

	invalid := map[string]struct {
		body   string
		wantIn string
	}{
		"emptyBody":       {``, "body"},
		"notAnObject":     {`[1]`, "shape"},
		"missingRevision": {`{"actions": []}`, "manifestRevision"},
		"zeroRevision":    {`{"manifestRevision": 0}`, "manifestRevision"},
		"missingCode":     {`{"manifestRevision": 1, "actions": [{"name": "x"}]}`, "actions[0].code"},
		"duplicateCode": {`{"manifestRevision": 1,
			"actions": [{"code": "m.x"}, {"code": "m.x"}]}`, "actions[1].code"},
		"badDanger": {`{"manifestRevision": 1,
			"actions": [{"code": "m.x", "danger": "extreme"}]}`, "danger"},
		"unsupportedKeyword": {`{"manifestRevision": 1,
			"actions": [{"code": "m.x", "params": {"pattern": "^a"}}]}`, "actions[0].params"},
		"badEventPayload": {`{"manifestRevision": 1,
			"events": [{"id": "m.e", "payload": {"format": "uuid"}}]}`, "events[0].payload"},
		"duplicateContext": {`{"manifestRevision": 1,
			"contexts": [{"id": "t"}, {"id": "t"}]}`, "contexts[1].id"},
		"longCode": {`{"manifestRevision": 1,
			"actions": [{"code": "` + strings.Repeat("x", 129) + `"}]}`, "code"},
	}
	for name, tc := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			_, faults := validateManifest(json.RawMessage(tc.body))
			if len(faults) == 0 {
				t.Fatalf("validateManifest(%s) found nothing", tc.body)
			}
			all := ""
			for _, fault := range faults {
				all += fault.Path + " " + fault.Message + "\n"
			}
			if !strings.Contains(all, tc.wantIn) {
				t.Errorf("faults do not mention %q:\n%s", tc.wantIn, all)
			}
		})
	}
}

func TestNewManifestRejectCapsAndNames(t *testing.T) {
	// A manifest wrong in 500 ways still produces a bounded notice that names
	// the rejected envelope and revision.
	actions := make([]string, 0, 500)
	for range 500 {
		actions = append(actions, `{"name": "no code"}`)
	}
	body := json.RawMessage(`{"manifestRevision": 9, "actions": [` + strings.Join(actions, ",") + `]}`)
	_, faults := validateManifest(body)
	if len(faults) <= maxManifestFaults {
		t.Fatalf("want more than %d faults to exercise the cap, got %d", maxManifestFaults, len(faults))
	}

	notice := newManifestReject("envelope-9", body, faults)
	var decoded manifestRejectBody
	if err := json.Unmarshal(notice.Body, &decoded); err != nil {
		t.Fatalf("decode notice: %v", err)
	}
	if decoded.EnvelopeID != "envelope-9" || decoded.ManifestRevision != 9 {
		t.Errorf("notice names %q revision %d, want envelope-9 revision 9",
			decoded.EnvelopeID, decoded.ManifestRevision)
	}
	if len(decoded.Errors) != maxManifestFaults {
		t.Errorf("notice carries %d errors, want the cap of %d", len(decoded.Errors), maxManifestFaults)
	}
}

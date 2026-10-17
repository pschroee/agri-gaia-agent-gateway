// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"encoding/json"
	"testing"
)

// The body of a delete call is what CVAT 2.4.2 validates before it deletes anything (V3b): CVAT reads it
// as LabeledDataSerializer, so every element carries frame and label_id, a shape its type and a track the
// list of its shapes. A bare id — the form the gateway sent until 17.10.2026 — answers HTTP 400 with
// {"frame":["This field is required."],"label_id":[...]} and nothing is deleted at all.
func TestAnnotationsDeleteBody(t *testing.T) {
	body, err := json.Marshal(AnnotationsDelete{Version: 0,
		Tags:   []DeleteTag{{ID: 11, Frame: 25, LabelID: 1}},
		Shapes: []DeleteShape{{ID: 12, Frame: 26, LabelID: 2, Type: "rectangle"}},
		Tracks: []DeleteTrack{{ID: 13, Frame: 27, LabelID: 1, Shapes: []any{}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":0,` +
		`"tags":[{"id":11,"frame":25,"label_id":1}],` +
		`"shapes":[{"id":12,"frame":26,"label_id":2,"type":"rectangle"}],` +
		`"tracks":[{"id":13,"frame":27,"label_id":1,"shapes":[]}]}`
	if string(body) != want {
		t.Errorf("delete body:\n got %s\nwant %s", body, want)
	}
	// A delete call without objects still names all three lists: CVAT's serializer takes them as empty
	// lists, and an element without frame or label_id would be refused.
	body, err = json.Marshal(AnnotationsDelete{Version: 0}.Normalized())
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"version":0,"tags":[],"shapes":[],"tracks":[]}` {
		t.Errorf("empty delete body: %s", body)
	}
}

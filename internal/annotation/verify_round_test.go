// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"encoding/json"
	"testing"
)

// The report of a round names the round it belongs to and the figures of the round before it (V3c): the
// user sees whether the correction helped.
func TestVerifyPreviousAndDelta(t *testing.T) {
	e, chatID, fillID := filledTask(t)
	first := e.verifyOf(t, chatID, fillID, false)
	var before verifyAnswer
	if err := json.Unmarshal([]byte(first.Result), &before); err != nil {
		t.Fatal(err)
	}
	if before.Round != 0 || before.Previous != nil || before.Delta != nil {
		t.Fatalf("the initial measurement carries a round or a comparison: %+v", before)
	}
	if before.Overall.Accuracy == nil || *before.Overall.Accuracy == 0 {
		t.Fatalf("the initial measurement has no figure: %+v", before.Overall)
	}
	// The first correction round: the fill writes what the function answers, and the answers are wrong
	// from here on, so that the second measurement is worse than the first.
	roundID := correctionFill(t, e, chatID)
	wrong := map[int]string{}
	for i := 0; i < 30; i++ {
		wrong[i] = `[{"label":"1","raw_label":"class_1","type":"tag","confidence":"0.9000","scores":[0.1,0.9]}]`
	}
	// The measurement of the round is the second one.
	e.fc.setAnswers(wrong)
	second := e.verifyOf(t, chatID, roundID, false)
	var after verifyAnswer
	if err := json.Unmarshal([]byte(second.Result), &after); err != nil {
		t.Fatal(err)
	}
	if after.Round != 1 {
		t.Errorf("the round of the second measurement: %d", after.Round)
	}
	if after.Previous == nil || after.Previous.SourceID != fillID || after.Previous.Round != 0 {
		t.Fatalf("the previous round is not named: %+v", after.Previous)
	}
	if after.Previous.Accuracy == nil || *after.Previous.Accuracy != *before.Overall.Accuracy {
		t.Errorf("the previous figure is not the one of the first measurement: %+v", after.Previous)
	}
	if after.Delta == nil {
		t.Fatal("the report carries no change")
	}
	if after.Overall.Accuracy == nil {
		t.Fatalf("the second measurement has no figure: %+v", after.Overall)
	}
	if want := *after.Overall.Accuracy - *before.Overall.Accuracy; after.Delta.Score != want {
		t.Errorf("delta %v, want %v", after.Delta.Score, want)
	}
	if after.Delta.PerClassF1 == nil {
		t.Errorf("the change per class is missing: %+v", after.Delta)
	}
}

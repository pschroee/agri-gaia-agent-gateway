// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package annotation

import (
	"context"
	"encoding/json"
	"fmt"

	"agw/internal/assess"
	"agw/internal/store"
)

// The correction loop of V3c (Active Learning light). A pre-annotation of a task begins with one run or
// one fill; every further one on the same task in the same chat is a correction round, and the agent may
// run at most MaxRounds of them. A round also has to be earned: it counts only while the round before it
// made the figure on the validation images better, and it is refused while those images are too few to
// compare anything on. The held-out images are never part of a round (E22) — a round after a final
// verdict would be a choice made by the outcome.
//
// The rules stand here once, with the reason they have, in the manner of verdict.go: the judgement of a
// round and the gate before it are pure functions of what the store reports, so that they can be tested
// without CVAT and without a function, and so that two reports cannot judge differently.

const (
	// MaxRounds is how many correction rounds a task may have in one chat. A round costs two compute
	// calls (the fill and the interim verification); three rounds plus the initial pre-annotation with
	// its verification are eight, more than the compute limit of a chat (AGW_COMPUTE_MAX_PER_CHAT, six)
	// allows. The round number makes that limit understandable instead of hitting it unexplained.
	MaxRounds = 3
	// MaxRetrains is how often a task may take a newly trained model into its loop. A retraining costs a
	// training and the build of an image (minutes) and needs new manual annotations to train on; once is
	// what the data of a quiet correction round supports.
	MaxRetrains = 1
)

// RoundScore is the figure of one round of the loop (V3c): the accuracy of a classification or the macro
// F1 of a detector on the validation images — the same figure the verdict compares (scoreOf).
type RoundScore struct {
	// Round is the correction round the figure belongs to (0: the initial pre-annotation, the figure the
	// first round has to improve on).
	Round int `json:"round"`
	// Score is the figure the rounds are compared with (accuracy or macro F1).
	Score float64 `json:"score"`
}

// RoundComparison is the interim figure of the round before this one (V3c). The report of a correction
// round names it next to its own figures, so that the user sees whether the correction helped.
type RoundComparison struct {
	// SourceID is the run or the fill the previous interim figure was measured on.
	SourceID string `json:"source_id"`
	// Round is the correction round that figure belongs to (0: the initial pre-annotation).
	Round int `json:"round"`
	// Verdict and the two overall figures are what that measurement reported; the field that does not
	// apply to the kind stands null.
	Verdict  string   `json:"verdict"`
	Accuracy *float64 `json:"accuracy,omitempty"`
	MacroF1  *float64 `json:"macro_f1,omitempty"`
	// Score is the figure the rounds are compared with.
	Score float64 `json:"score"`
}

// RoundDelta is the change against the round before (V3c): the figure the rounds are compared with and,
// per class, the change of F1, so that a report shows which class moved.
type RoundDelta struct {
	// Score is the change of the figure the rounds are compared with (accuracy or macro F1).
	Score float64 `json:"score"`
	// Accuracy and MacroF1 are the change of both overall figures; both reports have to carry a figure for
	// one of them to stand here.
	Accuracy float64 `json:"accuracy,omitempty"`
	MacroF1  float64 `json:"macro_f1,omitempty"`
	// PerClassF1 is the change of F1 per class that both measurements carry.
	PerClassF1 map[string]float64 `json:"per_class_f1,omitempty"`
}

// CorrectionState is what the rules of the loop rest on (V3c): the counters of one task in one chat,
// whether its held-out images were used, the size of its validation set and the two newest interim
// figures of the task.
type CorrectionState struct {
	// Rounds and Retrains are the counters of the task in this chat (annotation_tasks).
	Rounds   int
	Retrains int
	// Final: the held-out images of the task were already measured in this chat (E22).
	Final bool
	// Validation is how many images the validation set of the task has now, ValidationBefore how many the
	// last interim measurement rested on. More than before means the user annotated by hand since then,
	// and a round below the minimum is allowed again — it rests on new data.
	Validation       int
	ValidationBefore int
	// Prev and Cur are the two newest interim figures of the task, oldest first. Prev is nil while only
	// one of them stands: the round right after the initial pre-annotation has nothing to compare with.
	Prev *RoundScore
	Cur  *RoundScore
}

// RoundRefusal decides whether another correction round of a task may start (V3c). It is a pure function
// of what the store reports: the empty answer means the round may run, every other one names the reason
// it may not, word for word ready for the agent's reply. The order of the rules is the order of the
// report: the held-out images come first (nothing may be chosen by them), then the comparison the round
// rests on, then the data it needs, and at last the counter.
func RoundRefusal(st CorrectionState) string {
	if st.Final {
		return "no correction round after the final verdict on this task in this chat; the held-out images have to stay untouched. Annotate more images manually and start a new chat to correct again"
	}
	if st.Prev != nil && st.Cur != nil {
		// The rule of the training loop (R0.8), the same threshold and the same sentence: below the
		// smallest improvement that is more than noise on a handful of images the loop stops and reports
		// instead of trying another setting.
		if ok, msg := assess.MayContinue(st.Prev.Score, st.Cur.Score, st.Cur.Round); !ok {
			return msg
		}
	}
	if st.Validation < MinValidationImages && !st.grew() {
		// Without enough validation images a round has nothing to measure itself against; the number is
		// the one the interim verdict uses (MinValidationImages). Annotations the user added by hand since
		// the last measurement are new data, and a round on those is no longer blind.
		return fmt.Sprintf("too few validation images (%d of at least %d); ask the user to annotate more images (round needs new manual data)",
			st.Validation, MinValidationImages)
	}
	if st.Rounds >= MaxRounds {
		return fmt.Sprintf("at most %d correction rounds per task in a chat (%d done); report the result of the last round and stop",
			MaxRounds, st.Rounds)
	}
	return ""
}

// grew reports whether the validation set stands above what the last interim measurement rested on: the
// user annotated images by hand since then. Without a measurement nothing has grown — an empty set stays
// empty until the user works on it.
func (st CorrectionState) grew() bool {
	return st.ValidationBefore > 0 && st.Validation > st.ValidationBefore
}

// RetrainRefusal decides whether a function may be built while the correction loops of a chat run (V3c)
// and names the tasks the retraining counts for. Building a function of a model that was trained before
// any correction is the normal way of V2 and is no retraining, so a task without a round is left out;
// a task whose loop runs may take one newly trained model (MaxRetrains).
func RetrainRefusal(rows []store.AnnotationTaskCounters) (tasks []int64, msg string) {
	tasks = []int64{}
	for _, r := range rows {
		if r.Rounds < 1 {
			continue
		}
		if r.Retrains >= MaxRetrains {
			return nil, fmt.Sprintf("at most %d retraining per task in a chat (task %d has %d); report the result of the last round and stop",
				MaxRetrains, r.TaskID, r.Retrains)
		}
		tasks = append(tasks, r.TaskID)
	}
	return tasks, ""
}

// overallScore is the one figure the rounds are compared with (V3c): the accuracy of a classification,
// the macro F1 of a detector — a task carries one of them.
func overallScore(o Overall) float64 {
	if o.Accuracy != nil {
		return *o.Accuracy
	}
	if o.MacroF1 != nil {
		return *o.MacroF1
	}
	return 0
}

// ---------------------------------------------------------------------------
// The calls of the service
// ---------------------------------------------------------------------------

// beginRound checks whether the next fill or run of a task may start and counts it as a correction round
// (V3c). The initial pre-annotation of a task in a chat is no round: it runs unasked and is not counted.
// Every further one is measured against the rules of the loop and counted before it runs, so that a round
// the gateway refused cannot be started over and over.
func (s *Service) beginRound(ctx context.Context, chatID string, taskID int64) error {
	prior, err := s.St.PreAnnotated(ctx, chatID, taskID)
	if err != nil {
		return err
	}
	if prior == 0 {
		return nil
	}
	st, err := s.correctionState(ctx, chatID, taskID)
	if err != nil {
		return err
	}
	if msg := RoundRefusal(st); msg != "" {
		return deny("%s", msg)
	}
	_, err = s.St.AddCorrectionRound(ctx, chatID, taskID)
	return err
}

// correctionState reads what the rules of the loop rest on (V3c): the counters of the task in this chat,
// the size of its validation set and the two newest interim figures of the task.
func (s *Service) correctionState(ctx context.Context, chatID string, taskID int64) (CorrectionState, error) {
	counters, err := s.St.AnnotationTaskCountersOf(ctx, chatID, taskID)
	if err != nil {
		return CorrectionState{}, err
	}
	st := CorrectionState{Rounds: counters.Rounds, Retrains: counters.Retrains, Final: counters.Final}
	frames, err := s.St.FramesWithPart(ctx, taskID, store.SplitValidation)
	if err != nil {
		return CorrectionState{}, err
	}
	st.Validation = len(frames)
	results, err := s.St.VerifyResultsOfTask(ctx, chatID, taskID, 2)
	if err != nil {
		return CorrectionState{}, err
	}
	// The results come newest first; the newest one is the round before the one that is asking.
	if len(results) > 0 {
		st.Cur = roundScoreOf(results[0])
	}
	if len(results) > 1 {
		st.Prev = roundScoreOf(results[1])
	}
	if cur := measuredOf(results); cur > 0 {
		st.ValidationBefore = cur
	}
	return st, nil
}

// beginRetrain checks the retraining rule of the loop (V3c) and counts the retraining.
func (s *Service) beginRetrain(ctx context.Context, chatID string) error {
	rows, err := s.St.AnnotationTaskCountersOfChat(ctx, chatID)
	if err != nil {
		return err
	}
	tasks, msg := RetrainRefusal(rows)
	if msg != "" {
		return deny("%s", msg)
	}
	return s.St.AddRetrain(ctx, chatID, tasks)
}

// roundReport reads what the report of a verification has to name about the loop (V3c): the round the
// measurement belongs to and the figures of the round before it. The measurement itself is not part of
// it — the caller has it.
func (s *Service) roundReport(ctx context.Context, chatID string, taskID int64) (int, *RoundComparison, map[string]assess.ClassFigures, error) {
	counters, err := s.St.AnnotationTaskCountersOf(ctx, chatID, taskID)
	if err != nil {
		return 0, nil, nil, err
	}
	results, err := s.St.VerifyResultsOfTask(ctx, chatID, taskID, 1)
	if err != nil {
		return 0, nil, nil, err
	}
	if len(results) == 0 {
		return counters.Rounds, nil, nil, nil
	}
	var prev verifyRound
	if json.Unmarshal([]byte(results[0]), &prev) != nil {
		return counters.Rounds, nil, nil, nil
	}
	return counters.Rounds, prev.comparison(), prev.PerClass, nil
}

// roundDelta is the change of the figures against the round before (V3c); nil without a round to compare
// with, because a delta without a base would read as no change at all.
func roundDelta(prev *RoundComparison, prevClasses, curClasses map[string]assess.ClassFigures, cur Overall) *RoundDelta {
	if prev == nil {
		return nil
	}
	d := &RoundDelta{Score: overallScore(cur) - prev.Score}
	if cur.Accuracy != nil && prev.Accuracy != nil {
		d.Accuracy = *cur.Accuracy - *prev.Accuracy
	}
	if cur.MacroF1 != nil && prev.MacroF1 != nil {
		d.MacroF1 = *cur.MacroF1 - *prev.MacroF1
	}
	perClass := map[string]float64{}
	for class, cf := range curClasses {
		if before, ok := prevClasses[class]; ok {
			perClass[class] = cf.F1 - before.F1
		}
	}
	if len(perClass) > 0 {
		d.PerClassF1 = perClass
	}
	return d
}

// verifyRound is the part of the result of a verification the loop reads (V3c). The result is written by
// an older version of the gateway as well, so every field has to stand a missing one.
type verifyRound struct {
	SourceID string                         `json:"source_id"`
	Round    int                            `json:"round"`
	Verdict  string                         `json:"verdict"`
	Set      string                         `json:"set"`
	Measured int                            `json:"measured_images"`
	Overall  Overall                        `json:"overall"`
	PerClass map[string]assess.ClassFigures `json:"per_class"`
}

// comparison is the figure of this round as the next round names it (V3c).
func (r verifyRound) comparison() *RoundComparison {
	return &RoundComparison{SourceID: r.SourceID, Round: r.Round, Verdict: r.Verdict,
		Accuracy: r.Overall.Accuracy, MacroF1: r.Overall.MacroF1, Score: overallScore(r.Overall)}
}

// roundScoreOf reads the one figure of a result of a verification; a result the gateway cannot read
// carries no figure, and a round then counts as no improvement.
func roundScoreOf(raw string) *RoundScore {
	var r verifyRound
	if json.Unmarshal([]byte(raw), &r) != nil {
		return nil
	}
	return &RoundScore{Round: r.Round, Score: overallScore(r.Overall)}
}

// measuredOf is how many images the newest interim measurement rested on (V3c): the size the validation
// set had when the last round was compared. A final verdict rests on the held-out images and says
// nothing about the validation set, so it does not count here.
func measuredOf(results []string) int {
	for _, raw := range results {
		var r verifyRound
		if json.Unmarshal([]byte(raw), &r) != nil {
			continue
		}
		if r.Set == assess.SetValidation {
			return r.Measured
		}
	}
	return 0
}

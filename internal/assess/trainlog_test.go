// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"strings"
	"testing"
)

// Lightning prints its metrics as a table (with or without the box-drawing characters); the gateway
// reads the last value of a metric name and prefers the accuracy (R1.13).
func TestTrainLogLightning(t *testing.T) {
	log := `Epoch 0: 100%|██████████| 1/1 [00:00<00:00, 2.00it/s]
┏━━━━━━━━━━━━━━━━━━━━━━━━━━━┳━━━━━━━━━━━━━━━┓
┃      Test metric          ┃  DataLoader 0 ┃
┡━━━━━━━━━━━━━━━━━━━━━━━━━━━╇━━━━━━━━━━━━━━━┩
│      MulticlassAccuracy   │      0.2500   │
└───────────────────────────┴───────────────┘
Epoch 2: 100%|██████████| 1/1 [00:00<00:00, 2.00it/s]
┏━━━━━━━━━━━━━━━━━━━━━━━━━━━┳━━━━━━━━━━━━━━━┓
┃      Test metric          ┃  DataLoader 0 ┃
┡━━━━━━━━━━━━━━━━━━━━━━━━━━━╇━━━━━━━━━━━━━━━┩
│      MulticlassAccuracy   │      0.9583   │
│      MulticlassF1Score    │      0.9500   │
└───────────────────────────┴───────────────┘
`
	tl := ParseTrainLog(log)
	if !tl.Have {
		t.Fatal("no figure found")
	}
	if tl.Figure.Name != "MulticlassAccuracy" || tl.Figure.Value != 0.9583 {
		t.Fatalf("figure: %+v", tl.Figure)
	}
	if tl.NaN {
		t.Error("a nan is reported although the log has none")
	}
	// A table without borders works as well, and a shorter name is taken when the prefixed one is absent.
	tl = ParseTrainLog("Accuracy: 0.8125\nLoss: 0.3\n")
	if !tl.Have || tl.Figure.Name != "Accuracy" || tl.Figure.Value != 0.8125 {
		t.Fatalf("without borders: %+v", tl)
	}
	// A log without a figure is not read as one.
	if tl := ParseTrainLog("Epoch 1: loss 0.5\n"); tl.Have {
		t.Errorf("a log without a figure gave one: %+v", tl)
	}
	if f := platformFigure(TrainLog{}, 30); f != nil {
		t.Errorf("a log without a figure gave a platform figure: %+v", f)
	}
}

// Ultralytics prints one line per split; the row for all classes carries the figure the platform's own
// score pattern reads (R1.13).
func TestTrainLogYOLO(t *testing.T) {
	log := `                 Class     Images  Instances          P          R      mAP50   mAP50-95
                   all        120        120      0.991      0.983       0.99      0.971
`
	tl := ParseTrainLog(log)
	if !tl.Have || tl.Figure.Name != "mAP50-95" || tl.Figure.Value != 0.971 {
		t.Fatalf("figure: %+v", tl)
	}
	if tl.Images != 120 || tl.Instances != 120 {
		t.Errorf("images and instances: %d %d", tl.Images, tl.Instances)
	}
}

// A nan loss is reported so that a report can say the training did not converge (R1.13).
func TestTrainLogNaN(t *testing.T) {
	if tl := ParseTrainLog("Epoch 1: train_loss=nan\n"); !tl.NaN {
		t.Error("a nan loss is not reported")
	}
	if tl := ParseTrainLog("Epoch 1: loss=0.5\n"); tl.NaN {
		t.Error("a log without a nan loss reports one")
	}
	if tl := ParseTrainLog("Model has nan in the name\n"); tl.NaN {
		t.Error("a nan without a loss counts as one")
	}
}

// The note of the platform's figure says what it is worth and carries no "platform figure" prefix (H114).
func TestPlatformFigureNote(t *testing.T) {
	note := PlatformFigureNote(120)
	if !strings.HasPrefix(note, "measured by the template on its own internal test split of about 24 images") {
		t.Errorf("note: %q", note)
	}
	if !strings.Contains(note, "the gateway cannot see which images") {
		t.Errorf("note: %q", note)
	}
	if !strings.Contains(PlatformFigureNote(0), "about 0 images") {
		t.Errorf("without images: %q", PlatformFigureNote(0))
	}
}

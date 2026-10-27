// SPDX-FileCopyrightText: 2026 Philipp Schröer
// SPDX-License-Identifier: MIT

package titler

import (
	"errors"
	"strings"
	"testing"
)

const chatEMessage = "For a test of a failed start, propose ONE plan with exactly these four steps: 1. build_inference_image agw231-e11:v1 and then the rest"

func TestDetect(t *testing.T) {
	for _, c := range []struct{ text, want string }{
		{`Führe mit bash "sleep 60; echo fertig" aus.`, "de"},
		{"Liste mit agw-platform die Datensätze auf (nur Namen)", "de"},
		{"Antworte nur mit einem Wort.", "de"},
		{chatEMessage, "en"},
		{"Unzip the attached mnist-mini.zip (30 PNG images and annotations.xml) and upload its contents as a new dataset named x", "en"},
		{"Make platform model 3 (mnist_demo) a pre-annotation function with the annotation-function template", "en"},
		{"Reply with exactly: ok", "en"},
		{"ok", ""},
		{"x", ""},
		{"sleep 45", ""},
		{"agw-platform datasets", ""},
	} {
		if got := Detect(c.text); got != c.want {
			t.Errorf("Detect(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

func TestTarget(t *testing.T) {
	for _, c := range []struct{ text, lang, tag, name string }{
		{"Liste die Datensätze auf, bitte", "en-US", "de", "German"},
		{"What are you doing with the files?", "de-DE", "en", "English"},
		{"ok", "de-DE", "de", "German"},
		{"ok", "", "en", "English"},
		{"ok", "fr-CA", "fr", "French"},
		{"ok", "zh-CN", "zh", "Chinese"},
		{"ok", "qaa", "en", "English"},
	} {
		tag, name := Target(c.text, c.lang)
		if tag != c.tag || name != c.name {
			t.Errorf("Target(%q, %q) = %s/%s, want %s/%s", c.text, c.lang, tag, name, c.tag, c.name)
		}
	}
}

func TestCheckTitle(t *testing.T) {
	han := "上传 mnist-mini 并训练 EfficientNet"
	err := checkTitle(han, "Unzip the attached mnist-mini.zip and upload it", "en", "English")
	if !errors.Is(err, ErrLanguage) || !strings.Contains(err.Error(), "Han") {
		t.Errorf("Han title for English message: %v", err)
	}
	if err := checkTitle(han, "上传 数据集 并训练", "en", "English"); err != nil {
		t.Errorf("Han title, message contains Han: %v", err)
	}
	if err := checkTitle(han, "ok", "zh", "Chinese"); err != nil {
		t.Errorf("Han title, target zh: %v", err)
	}
	if err := checkTitle("Обучение модели", "Trainiere das Modell", "de", "German"); !errors.Is(err, ErrLanguage) {
		t.Errorf("Cyrillic title, target de: %v", err)
	}
	for _, title := range []string{"Plan für Inferenz-Image agw70-test", "MNIST-Modell als Vorannotation nutzen"} {
		err := checkTitle(title, "Take over the model and build an image", "en", "English")
		if !errors.Is(err, ErrLanguage) || !strings.Contains(err.Error(), "reads as German") {
			t.Errorf("German title %q for English target: %v", title, err)
		}
	}
	if err := checkTitle("Datensatz zählen", "Wie viele Bilder?", "de", "German"); err != nil {
		t.Errorf("German title, target de: %v", err)
	}
	if err := checkTitle("Train EfficientNet on mnist-mini", "Train it", "en", "English"); err != nil {
		t.Errorf("English title, target en: %v", err)
	}
}

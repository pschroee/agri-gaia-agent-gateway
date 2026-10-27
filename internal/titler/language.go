// SPDX-FileCopyrightText: 2026 Philipp Schröer
// SPDX-License-Identifier: MIT

package titler

import (
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

var germanWords = wordSet(`und der die das den dem des ein eine einen einem einer ist sind nicht nur mit für auf aus bei nach von zu zum
zur im als über vom beim ohne ich du mir mich mein meine dein wir ihr wie welche welcher welches wann warum bitte
oder auch noch dann wenn dass kannst kann soll mach führe lege zeig gib ändere hallo danke ja nein neuen neue alle`)

var englishWords = wordSet(`the and is are not only with for on from to of by at this that these it you me my your we what how which when
why please or then if can could should do does make run create list show give change hello hi thanks yes new all`)

func wordSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) })
}

// Detect tells German or English apart by function words (E2): "de", "en" or "" when unclear.
// One side needs at least one hit and at least twice as many as the other.
func Detect(text string) string {
	de, en := 0, 0
	for _, w := range words(text) {
		if germanWords[w] || strings.ContainsAny(w, "äöüß") {
			de++
		}
		if englishWords[w] {
			en++
		}
	}
	switch {
	case de >= 1 && de >= 2*en:
		return "de"
	case en >= 1 && en >= 2*de:
		return "en"
	}
	return ""
}

// Target is the title's language: the message's (Detect), else the base of the chat's browser language,
// else English. tag is the base ("de"), name its English name ("German").
func Target(text, chatLang string) (tag, name string) {
	switch Detect(text) {
	case "de":
		return "de", "German"
	case "en":
		return "en", "English"
	}
	if chatLang != "" {
		if t, err := language.Parse(chatLang); err == nil {
			b, _ := t.Base()
			if n := display.English.Languages().Name(b); n != "" {
				return b.String(), n
			}
		}
	}
	return "en", "English"
}

// scriptTables maps a language.Script to the unicode tables its letters come from.
var scriptTables = map[string][]*unicode.RangeTable{
	"Latn": {unicode.Latin},
	"Cyrl": {unicode.Cyrillic},
	"Grek": {unicode.Greek},
	"Hans": {unicode.Han},
	"Hant": {unicode.Han},
	"Jpan": {unicode.Han, unicode.Hiragana, unicode.Katakana},
	"Kore": {unicode.Hangul, unicode.Han},
	"Arab": {unicode.Arabic},
	"Hebr": {unicode.Hebrew},
	"Thai": {unicode.Thai},
	"Deva": {unicode.Devanagari},
}

// checkedScripts are the non-Latin scripts that checkTitle looks for, with names for the error.
var checkedScripts = []struct {
	name  string
	table *unicode.RangeTable
}{
	{"Han", unicode.Han}, {"Hiragana", unicode.Hiragana}, {"Katakana", unicode.Katakana}, {"Hangul", unicode.Hangul},
	{"Cyrillic", unicode.Cyrillic}, {"Greek", unicode.Greek}, {"Arabic", unicode.Arabic}, {"Hebrew", unicode.Hebrew},
	{"Thai", unicode.Thai}, {"Devanagari", unicode.Devanagari},
}

// checkTitle rejects a title in another script than the target's or the message's (E5), or in the other of de/en (E6).
func checkTitle(title, text, tag, name string) error {
	sc, _ := language.Make(tag).Script()
	allowed := scriptTables[sc.String()]
	if allowed != nil {
		for _, r := range title {
			if !unicode.IsLetter(r) || unicode.Is(unicode.Latin, r) {
				continue
			}
			if unicode.IsOneOf(allowed, r) || strings.ContainsRune(text, r) {
				continue
			}
			for _, s := range checkedScripts {
				if unicode.Is(s.table, r) {
					return fmt.Errorf("%w: wanted %s, found %s script", ErrLanguage, name, s.name)
				}
			}
		}
	}
	if (tag == "de" || tag == "en") && Detect(title) != "" && Detect(title) != tag {
		other := "German"
		if tag == "de" {
			other = "English"
		}
		return fmt.Errorf("%w: wanted %s, reads as %s", ErrLanguage, name, other)
	}
	return nil
}

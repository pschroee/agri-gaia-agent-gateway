// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
)

// The backend writes every key and value of train_config into a shell command without checking it
// (model-training/common/config_to_args.py) and evaluates that command
// (common/docker-entrypoint.sh: `eval "time python train.py $(python config_to_args.py)"`), while
// the training container runs privileged on the host. The check here keeps shell code out of
// train_config (issue #93). It runs in Normalize, so the tool, the raw `request` and the REST
// binding go through it alike, with and without a delegation (like the field rules, issue #65).

// The positive lists of keys and values; the option names come from the four installed templates.
var (
	// trainKeyRe allows the option names the templates use (dashes and underscores), but no shell
	// metacharacter. trainValueRe allows the values, including the `0,1` of `device`, but no
	// whitespace and no shell metacharacter.
	trainKeyRe   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,40}$`)
	trainValueRe = regexp.MustCompile(`^[A-Za-z0-9_.,:/+-]{0,100}$`)
)

// trainListMax is the largest list a train_config value may be (only `freeze` is one).
const trainListMax = 32

// CheckTrainConfig returns the reason that keeps the body's train_config from going out, or ""
// when it may go. The body is read token by token, because encoding/json keeps only the last of
// duplicate keys and so does the platform (code review K2). A body without train_config passes:
// there is no value that could reach the shell, and the platform refuses it on its own.
func CheckTrainConfig(body []byte) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return "" // not valid JSON: Normalize refuses the body right after this check
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return ""
	}
	seenConfig := false
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return ""
		}
		key, _ := keyTok.(string)
		if key != "train_config" {
			// provider, architecture, category, dataset_id, export_config … never reach the shell.
			var skip any
			if err := dec.Decode(&skip); err != nil {
				return ""
			}
			continue
		}
		if seenConfig {
			return fmt.Sprintf("train_config: duplicate key %q", "train_config")
		}
		seenConfig = true
		if why := checkTrainConfigValue(dec); why != "" {
			return why
		}
	}
	return ""
}

// checkTrainConfigValue reads the value of the train_config member, which must be an object.
func checkTrainConfigValue(dec *json.Decoder) string {
	tok, err := dec.Token()
	if err != nil {
		return ""
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "train_config: object expected"
	}
	return checkTrainConfigObject(dec)
}

// checkTrainConfigObject reads the option list of train_config and checks every key and value.
func checkTrainConfigObject(dec *json.Decoder) string {
	seen := map[string]bool{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return ""
		}
		key, ok := keyTok.(string)
		if !ok {
			return "train_config: key not allowed (letters, digits, dashes and underscores only)"
		}
		if !trainKeyRe.MatchString(key) {
			return fmt.Sprintf("train_config: key %q not allowed (letters, digits, dashes and underscores only)", key)
		}
		if seen[key] {
			return fmt.Sprintf("train_config: duplicate key %q", key)
		}
		seen[key] = true
		if why := checkTrainValue(dec, key); why != "" {
			return why
		}
	}
	if _, err := dec.Token(); err != nil { // the closing brace of the object
		return ""
	}
	return ""
}

// checkTrainValue checks one value: a number, a truth value, null, an allowed string or a list of
// them; never an object.
func checkTrainValue(dec *json.Decoder, key string) string {
	tok, err := dec.Token()
	if err != nil {
		return ""
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '[' {
			return checkTrainList(dec, key)
		}
		return "train_config: nested objects are not allowed"
	case string:
		if !trainValueRe.MatchString(t) {
			return trainValueMsg(key)
		}
	case json.Number, bool, nil:
		// allowed: numbers, true/false and null
	default:
		return trainValueMsg(key)
	}
	return ""
}

// checkTrainList checks a list value: at most trainListMax elements, each a scalar like above.
func checkTrainList(dec *json.Decoder, key string) string {
	n := 0
	for dec.More() {
		n++
		if n > trainListMax {
			return trainValueMsg(key)
		}
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		switch t := tok.(type) {
		case json.Delim:
			if t == '{' {
				return "train_config: nested objects are not allowed"
			}
			return trainValueMsg(key) // a list inside a list is not a scalar
		case string:
			if !trainValueRe.MatchString(t) {
				return trainValueMsg(key)
			}
		case json.Number, bool, nil:
			// allowed: numbers, true/false and null
		default:
			return trainValueMsg(key)
		}
	}
	if _, err := dec.Token(); err != nil { // the closing bracket of the list
		return ""
	}
	return ""
}

func trainValueMsg(key string) string {
	return fmt.Sprintf("train_config.%s: value not allowed (numbers, true/false, null or text of letters, digits, _.,:/+- up to 100 characters)", key)
}

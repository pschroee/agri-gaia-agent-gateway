// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// The summary of a function the agent builds from a model (V2) lives in its own file: init appends
// it, so that the switch in summary.go stays what it is. The preview names what the call will do and
// what the function will be: the code comes from the gateway, the model from the platform, and the
// function stays for every CVAT user.
func init() { registerDescriber(describeAnnotationFunction) }

// functionTemplateVersion is the version of the templates of internal/annotation (its
// TemplateVersion): the summary names it, so the two must say the same.
const functionTemplateVersion = 1

// describeAnnotationFunction answers the build of a function; every other call of the feature keeps
// the built-in template.
func describeAnnotationFunction(c SummaryCall, s *summarizer) (string, []string, bool) {
	if c.Method != http.MethodPost || c.Path != AnnotationPrefix+"/functions" {
		return "", nil, false
	}
	mid := scalarText(s.body["model_id"])
	kind, _ := s.body["kind"].(string)
	if mid == "" || kind == "" {
		return "", nil, false // a call the service refuses: the generic template lists its fields
	}
	s.usedBody["model_id"], s.usedBody["kind"] = true, true

	// What the model is called and what it can do comes from the platform (reading GETs only); a
	// model an earlier step of a plan creates does not exist yet.
	var model map[string]any
	readErr := ""
	if p, isPh := placeholder(mid); isPh {
		readErr = fmt.Sprintf("created by step %d", p)
	} else if plainID(mid) {
		model, readErr = s.object("/models/" + mid)
	}
	head := fmt.Sprintf("Builds a CVAT pre-annotation function from %s with template %s v%d",
		s.named("model", mid), kind, functionTemplateVersion)
	var details []string
	if readErr != "" && !strings.HasPrefix(readErr, "created by") {
		details = append(details, "Model not readable: "+readErr)
	}
	details = append(details, functionClassLine(s, kind, model))
	details = append(details, functionInputLine(s, model))
	details = append(details,
		"The model file must be ONNX. The gateway reads it after this approval, gives the function the name agw-<user>-m<model id>-<the first eight hex digits of that file> and sends a fixed template of the gateway with it: the agent writes no code here.",
		"The function is visible to and usable by every CVAT user of the platform, and the agent cannot remove it (ask the operator).",
		"The build runs on the platform and takes several minutes; annotation_functions shows its state, \"ready\" means it can be run.")
	return head, details, true
}

// functionClassLine names the classes the function will carry: the ones the call sends, else the
// output_labels of the model, else the class_i list the service derives from output_shape.
func functionClassLine(s *summarizer, kind string, model map[string]any) string {
	if v, ok := s.body["labels"]; ok {
		s.usedBody["labels"] = true
		if list, isList := v.([]any); isList && len(list) > 0 {
			return "Classes (labels, in the order the model outputs them): " + textList(list)
		}
	}
	if list, _ := model["output_labels"].([]any); len(list) > 0 {
		return "Classes (output_labels of the model, in the order it outputs them): " + textList(list)
	}
	if count := lastShapeNumber(model); count > 0 && kind != "detection" {
		return fmt.Sprintf("Classes: the call sends none and the model names none; the service names them class_0 … class_%d from output_shape.", count-1)
	}
	return "Classes: the call sends none and the model names none; without class names a detector cannot be built (a classification model would get class_i from output_shape)."
}

// functionInputLine names the square input of the function: what the call sends, else the side of
// the model's input, else the default of the kind.
func functionInputLine(s *summarizer, model map[string]any) string {
	if v, ok := s.body["input_size"]; ok {
		s.usedBody["input_size"] = true
		if n := scalarText(v); n != "" {
			return "Input: " + n + "x" + n + " pixels (input_size of the call)"
		}
	}
	shape, _ := model["input_shape"].([]any)
	if len(shape) == 4 {
		if side := scalarText(shape[2]); side != "" && side == scalarText(shape[3]) {
			return "Input: " + side + "x" + side + " pixels (input_shape of the model)"
		}
	}
	return "Input: the service takes the input_shape of the model, else 224x224 (classification) or 640x640 (detection) pixels."
}

// lastShapeNumber is the last value of the model's output_shape, as the service reads it.
func lastShapeNumber(model map[string]any) int {
	shape, _ := model["output_shape"].([]any)
	if len(shape) == 0 {
		return 0
	}
	n, err := strconv.Atoi(scalarText(shape[len(shape)-1]))
	if err != nil {
		return 0
	}
	return n
}

// textList renders a list of JSON values as one line.
func textList(list []any) string {
	parts := make([]string, 0, len(list))
	for _, v := range list {
		parts = append(parts, scalarText(v))
	}
	return strings.Join(parts, ", ")
}

// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"encoding/json"
	"strings"
	"testing"
)

// The default train_config of the four installed templates, copied literally from
// services/backend/model-training/templates/*/*/*/config/config.jsonschema of the platform
// (field default), issue #93. Every one of them must pass the check.
var templateDefaults = map[string]string{
	"Torchvision/Classification/EfficientNet": `{"architecture":"efficientnet_v2_s","pretrained":true,` +
		`"epochs":50,"batch-size":32,"auto-batch-size":false,"img-size":300,"optimizer":"AdamW",` +
		`"lr-scheduler":"ReduceLROnPlateau","learning-rate":0.001,"lr-step-size":30,"auto-learning-rate":false,` +
		`"swa-learning-rate":0,"weight-decay":0.01,"momentum":0,"patience":0,"label-smoothing":0,` +
		`"metrics-average":"micro","train-split":0.8,"test-split":0.2}`,
	"Torchvision/Instance Segmentation/Mask R-CNN": `{"backbone":"resnet50_fpn","pretrained":true,"epochs":200,` +
		`"batch-size":16,"auto-batch-size":false,"img-size":800,"optimizer":"SGD","learning-rate":0.02,` +
		`"auto-learning-rate":false,"swa-learning-rate":0,"weight-decay":0.0001,"momentum":0.9,"patience":0,` +
		`"test-threshold":0.5,"train-split":0.8,"test-split":0.2}`,
	"Ultralytics/Object Detection/YOLOv11": `{"model":"yolo11m.pt","epochs":100,"patience":100,"batch":16,` +
		`"imgsz":640,"cache":false,"device":"cpu","workers":4,"pretrained":true,"optimizer":"auto","seed":0,` +
		`"deterministic":true,"single_cls":false,"rect":false,"multi_scale":false,"cos_ls":false,` +
		`"close_mosaic":10,"amp":true,"freeze":[0],"lr0":0.01,"lrf":0.01,"momentum":0.937,"weight_decay":0.0005,` +
		`"warmup_epochs":3.0,"warmup_momentum":0.8,"warmup_bias_lr":0.1,"box":7.5,"cls":0.5,"dfl":1.5,"nbs":64,` +
		`"dropout":0.0,"train-split":0.8,"test-split":0.2}`,
	"Ultralytics/Object Detection/YOLOv5": `{"model":"yolov5m.pt","epochs":300,"batch":16,"imgsz":640,` +
		`"rect":false,"cache":"ram","device":"cpu","multi_scale":0.0,"single-cls":false,"optimizer":"SGD",` +
		`"workers":4,"cos_lr":false,"patience":100,"freeze":[0],"train-split":0.8,"test-split":0.2}`,
}

// trainBody wraps a train_config object into the body of POST/PUT /train/config.
func trainBody(config string) json.RawMessage {
	return json.RawMessage(`{"provider":"Torchvision","architecture":"EfficientNet","category":"Classification",` +
		`"dataset_id":2,"train_config":` + config + `,"export_config":null}`)
}

func TestCheckTrainConfigAllowsTemplateDefaults(t *testing.T) {
	for name, config := range templateDefaults {
		if why := CheckTrainConfig(trainBody(config)); why != "" {
			t.Errorf("%s: refused: %s", name, why)
		}
	}
}

func TestCheckTrainConfigAllowsValues(t *testing.T) {
	allowed := []string{
		`{}`,                            // no option at all
		`{"epochs":50}`,                 // a number
		`{"pretrained":true}`,           // a truth value
		`{"momentum":null}`,             // null (config_to_args skips it)
		`{"optimizer":""}`,              // an empty string (config_to_args skips it)
		`{"device":"0,1"}`,              // a list of GPU ids, as a string
		`{"device":"cpu"}`,              // text
		`{"label-smoothing":0.0}`,       // a number, not a string
		`{"weights":"a_b.c-d/e:f+g"}`,   // every allowed special character
		`{"freeze":[0,1,2]}`,            // a list of numbers
		`{"names":["a","b"]}`,           // a list of strings
		`{"freeze":[]}`,                 // an empty list
		`{"mixed":[1,true,null,"cpu"]}`, // a list of scalars (null is skipped)
	}
	for _, config := range allowed {
		if why := CheckTrainConfig(trainBody(config)); why != "" {
			t.Errorf("%s: refused: %s", config, why)
		}
	}
	// A body without train_config passes (there is no value for the shell); the platform refuses it.
	for _, body := range []string{
		`{"dataset_id":2}`,
		``,
		`not json at all`,
		`[1,2,3]`,
	} {
		if why := CheckTrainConfig([]byte(body)); why != "" {
			t.Errorf("%q: refused: %s", body, why)
		}
	}
}

func TestCheckTrainConfigRefusesShell(t *testing.T) {
	// Every value here would become part of a shell command in the training container.
	values := []string{
		`"Adam; id"`,
		`"$(id)"`,
		"\"`id`\"",
		`"Adam | id"`,
		`"Adam && id"`,
		`"Adam > /tmp/x"`,
		`"Adam id"`,
		`"Adam\nid"`,
		`"Adam;id\n"`,
		`"Adam'id"`,
		`"Adam\tid"`,
		"\"Adam\\\"id\"",
		`"Adam#id"`,
		`"Adam%id"`,
		`"Adam$id"`,
		`"Adam*id"`,
		`"Adam?id"`,
		`"Adam~id"`,
		`"Adam(id)"`,
		`"Adam[id]"`,
	}
	for _, v := range values {
		if why := CheckTrainConfig(trainBody(`{"optimizer":` + v + `}`)); why == "" {
			t.Errorf("value %s: accepted", v)
		}
	}

	bad := map[string]string{
		"semicolon in key":         `{"optimizer;id":1}`,
		"space in key":             `{"opt imizer":1}`,
		"uppercase in key":         `{"Optimizer":1}`,
		"leading digit in key":     `{"1optimizer":1}`,
		"key too long":             `{"` + strings.Repeat("a", 42) + `":1}`,
		"nested object":            `{"optimizer":{"a":1}}`,
		"object in list":           `{"freeze":[{"a":1}]}`,
		"list in list":             `{"freeze":[[1]]}`,
		"duplicate key":            `{"epochs":1,"epochs":2}`,
		"duplicate key later evil": `{"epochs":1,"epochs":"Adam; id"}`,
		"value too long":           `{"optimizer":"` + strings.Repeat("a", 101) + `"}`,
		"list too long":            `{"freeze":[` + strings.Repeat("1,", 32) + `1]}`,
	}
	for name, config := range bad {
		if why := CheckTrainConfig(trainBody(config)); why == "" {
			t.Errorf("%s (%s): accepted", name, config)
		}
	}

	// train_config must be an object.
	if why := CheckTrainConfig(trainBody(`"Adam; id"`)); why == "" {
		t.Error("train_config as a string: accepted")
	}
	// A second train_config key is refused (encoding/json and the platform both keep the last one).
	if why := CheckTrainConfig([]byte(`{"train_config":{},"train_config":{"optimizer":"Adam; id"}}`)); why == "" {
		t.Error("duplicate train_config: accepted")
	}
}

// The messages name the place, so the agent learns what to fix.
func TestCheckTrainConfigMessages(t *testing.T) {
	cases := []struct{ config, want string }{
		{`{"optimizer;id":1}`, "key"},
		{`{"optimizer":"Adam; id"}`, "train_config.optimizer: value not allowed"},
		{`{"epochs":1,"epochs":2}`, "duplicate key"},
		{`{"optimizer":{"a":1}}`, "nested objects are not allowed"},
	}
	for _, c := range cases {
		why := CheckTrainConfig(trainBody(c.config))
		if !strings.Contains(why, c.want) {
			t.Errorf("%s: %q does not contain %q", c.config, why, c.want)
		}
	}
}

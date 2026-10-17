// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package assess

import (
	"regexp"

	"agw/internal/config"
)

// The image review rule of #111 (P14): a chat model may look at the pictures of a dataset only when the
// model runs on the user's own hardware, or when the dataset's name matches AGW_VISION_DATASET_PATTERN
// (a public test set). Everything else would send pictures of the user's animals to a cloud model.

// DefaultVisionDatasetPattern is the pattern unless AGW_VISION_DATASET_PATTERN says otherwise: the
// public example sets (mnist-…, test-…).
const DefaultVisionDatasetPattern = `^(mnist|test)[-_]`

// The reasons ImageReviewAllowed answers, word for word (R0.9).
const (
	// reasonTextOnly: the chat model does not read images at all.
	reasonTextOnly = "the current model reads text only"
	// reasonCloudDataset: the model runs on someone else's hardware and the dataset is not a public one.
	reasonCloudDataset = "images of this dataset may not go to the cloud model (AGW_VISION_DATASET_PATTERN)"
)

// VisionPattern compiles the configured pattern of datasets whose images may go to a cloud model;
// empty falls back to DefaultVisionDatasetPattern.
func VisionPattern(p string) (*regexp.Regexp, error) {
	if p == "" {
		p = DefaultVisionDatasetPattern
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, err
	}
	return re, nil
}

// ImageReviewAllowed reports whether a chat model may look at the images of a dataset (P14): the model
// must read images at all, and either it runs on the user's own hardware (self_hosted) or the dataset's
// name matches the pattern. reason names why not.
func ImageReviewAllowed(m config.Model, datasetName string, pattern *regexp.Regexp) (bool, string) {
	if !m.ReadsImages() {
		return false, reasonTextOnly
	}
	if m.SelfHosted {
		return true, ""
	}
	if pattern != nil && pattern.MatchString(datasetName) {
		return true, ""
	}
	return false, reasonCloudDataset
}

// ReviewAllowed is ImageReviewAllowed as the callers ask it (V3d, R4): it answers the rule of #111 for the
// chat's model, and it also says why there is no model to ask at all (no catalog, no model of the chat, a
// model the catalog does not know). One place for the rule and its reasons.
func ReviewAllowed(cat *config.Catalog, pattern *regexp.Regexp, chatModel, datasetName string) (bool, string) {
	if cat == nil {
		return false, "the model catalog is not set up"
	}
	if chatModel == "" {
		return false, "this chat has no model that could look at the images"
	}
	_, model, found := cat.Lookup(chatModel)
	if !found {
		return false, "the model " + chatModel + " is not in the catalog; whether it reads images is unknown"
	}
	return ImageReviewAllowed(model, datasetName, pattern)
}

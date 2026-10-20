// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// Package search holds the pure rules of a training search with a budget (issue #168): the search space and the
// check of a trial against it, the budget and the question whether another trial may start, the score of a trial
// and the parsers of the figures a training writes. It has no database and no network; the chat package joins it
// with the store and the platform.
package search

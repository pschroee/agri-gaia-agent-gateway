// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package config

import (
	"os"
	"time"
)

// Placeholder limits of an experiment run (X3.2–X3.4, X3.6). They are what the outline prescribes until the
// pilot runs (XE5) fix them from the budget and the number of runs: a run costs at most 0.50 USD, uses at most
// two million tokens and may take 90 minutes; temperature is 0.
const (
	DefaultExpMaxCostUSD  = 0.50
	DefaultExpMaxTokens   = 2_000_000
	DefaultExpMaxDuration = 90 * time.Minute
	DefaultExpTemperature = 0
)

// ExpLimits are the limits of an experiment run from the environment (X3).
type ExpLimits struct {
	MaxCostUSD     float64       // AGW_EXP_MAX_COST_USD
	MaxTokens      int64         // AGW_EXP_MAX_TOKENS
	MaxDuration    time.Duration // AGW_EXP_MAX_DURATION
	Temperature    float64       // AGW_EXP_TEMPERATURE
	PlaceholderSet bool          // none of the variables was set: the defaults apply
}

// expLimits reads the limits of an experiment run. The defaults are placeholders (PlaceholderSet); a value of 0
// turns a limit off (the check treats it as "no limit").
func expLimits() ExpLimits {
	const (
		costEnv = "AGW_EXP_MAX_COST_USD"
		tokEnv  = "AGW_EXP_MAX_TOKENS"
		durEnv  = "AGW_EXP_MAX_DURATION"
		tempEnv = "AGW_EXP_TEMPERATURE"
	)
	return ExpLimits{
		MaxCostUSD:  flt(costEnv, DefaultExpMaxCostUSD),
		MaxTokens:   max(0, int64(num(tokEnv, DefaultExpMaxTokens))),
		MaxDuration: dur(durEnv, DefaultExpMaxDuration),
		Temperature: flt(tempEnv, DefaultExpTemperature),
		PlaceholderSet: os.Getenv(costEnv) == "" && os.Getenv(tokEnv) == "" &&
			os.Getenv(durEnv) == "" && os.Getenv(tempEnv) == "",
	}
}

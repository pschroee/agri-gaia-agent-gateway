// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package config

import (
	"os"
	"time"
)

// Limits of an experiment run (X3.2–X3.4, X3.6), fixed from the pilot runs of 17.10.2026 (X7): a run costs
// at most 0.02 USD, uses at most 50 000 tokens (input + output, cache reads excluded) and may take 5 minutes;
// temperature is 0. The pilot's 90th percentile lay well below all three values; the full main runs may need
// higher limits once their tasks are frozen (XE5).
const (
	DefaultExpMaxCostUSD  = 0.02
	DefaultExpMaxTokens   = 50_000
	DefaultExpMaxDuration = 5 * time.Minute
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

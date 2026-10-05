package config

import (
	"fmt"
	"strings"
	"time"
)

// Tariff beschreibt zeitabhängige Preise eines Anbieters. Die Preise im
// Katalog bzw. in pis Register gelten als Spitzentarif; außerhalb der
// Spitzenzeiten wird mit OffPeakFactor multipliziert.
//
// Feiertage des Anbieters (bei DeepSeek chinesische Feiertage, an denen der
// Nebentarif gilt) sind nicht bekannt und werden als Spitzenzeit gerechnet;
// die Kosten sind damit eine obere Schranke.
type Tariff struct {
	PeakWindowsUTC []Window `json:"peak_windows_utc"`
	OffPeakFactor  float64  `json:"offpeak_factor"`
	Note           string   `json:"note,omitempty"`
	Source         string   `json:"source,omitempty"`    // URL der Preisseite des Anbieters
	Retrieved      string   `json:"retrieved,omitempty"` // Abrufdatum (ISO)
}

type Window struct {
	Days string `json:"days"` // "mon-fri", "sat,sun", "all"
	From string `json:"from"` // "HH:MM", einschließlich
	To   string `json:"to"`   // "HH:MM", ausschließlich
}

// Usage sind die Tokens einer Antwort, wie pi sie meldet.
type Usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
}

var dayNames = map[string]time.Weekday{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

func parseDays(s string) (map[time.Weekday]bool, error) {
	out := map[time.Weekday]bool{}
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "all" {
		for d := time.Sunday; d <= time.Saturday; d++ {
			out[d] = true
		}
		return out, nil
	}
	for _, part := range strings.Split(s, ",") {
		a, b, isRange := strings.Cut(strings.TrimSpace(part), "-")
		da, ok1 := dayNames[a]
		if !ok1 {
			return nil, fmt.Errorf("unbekannter Tag %q", a)
		}
		if !isRange {
			out[da] = true
			continue
		}
		db, ok2 := dayNames[b]
		if !ok2 {
			return nil, fmt.Errorf("unbekannter Tag %q", b)
		}
		for d := da; ; d = (d + 1) % 7 {
			out[d] = true
			if d == db {
				break
			}
		}
	}
	return out, nil
}

func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("Uhrzeit %q: erwartet HH:MM", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

func (t *Tariff) validate() error {
	if t.OffPeakFactor < 0 {
		return fmt.Errorf("offpeak_factor negativ")
	}
	for _, w := range t.PeakWindowsUTC {
		if _, err := parseDays(w.Days); err != nil {
			return err
		}
		from, err := parseClock(w.From)
		if err != nil {
			return err
		}
		to, err := parseClock(w.To)
		if err != nil {
			return err
		}
		if from >= to {
			return fmt.Errorf("Zeitfenster %s–%s: Beginn muss vor dem Ende liegen (über Mitternacht in zwei Fenster teilen)", w.From, w.To)
		}
	}
	return nil
}

// IsPeak prüft, ob der Zeitpunkt in eine Spitzenzeit fällt.
func (t *Tariff) IsPeak(at time.Time) bool {
	at = at.UTC()
	min := at.Hour()*60 + at.Minute()
	for _, w := range t.PeakWindowsUTC {
		days, _ := parseDays(w.Days)
		from, _ := parseClock(w.From)
		to, _ := parseClock(w.To)
		if days[at.Weekday()] && min >= from && min < to {
			return true
		}
	}
	return false
}

// Cost berechnet die Kosten einer Antwort in USD zum Zeitpunkt at.
// peak meldet, ob der Spitzentarif galt (ohne Tarif: immer Einheitspreis, peak=false).
func (c *Catalog) Cost(modelID string, u Usage, at time.Time) (cost float64, peak bool, ok bool) {
	prov, _, found := c.Lookup(modelID)
	if !found {
		return 0, false, false
	}
	p := c.EffectivePricing(modelID)
	if p == nil {
		return 0, false, false
	}
	cost = (float64(u.Input)*p.Input + float64(u.Output)*p.Output +
		float64(u.CacheRead)*p.CacheRead + float64(u.CacheWrite)*p.CacheWrite) / 1e6
	if prov.Tariff != nil && len(prov.Tariff.PeakWindowsUTC) > 0 {
		peak = prov.Tariff.IsPeak(at)
		if !peak {
			cost *= prov.Tariff.OffPeakFactor
		}
	}
	return cost, peak, true
}

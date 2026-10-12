// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package store

import "context"

type effectKey struct{}

// WithEffect attaches the effect class of a platform call to the context of its log entry.
func WithEffect(ctx context.Context, effect string) context.Context {
	return context.WithValue(ctx, effectKey{}, effect)
}

// EffectFrom returns the effect class from WithEffect (otherwise empty).
func EffectFrom(ctx context.Context) string {
	e, _ := ctx.Value(effectKey{}).(string)
	return e
}

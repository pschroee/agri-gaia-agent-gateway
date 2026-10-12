// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

import (
	"context"
	"encoding/json"
)

type invocationKey struct{}

// Invocation names the tool call (tool name and the arguments as the agent sent them) that a platform
// call belongs to. Only the socket sets it, when it runs a tool with steps; a raw request never carries it.
// The plans of issue #70 match tool steps on it instead of on the HTTP request the tool builds.
type Invocation struct {
	Tool string
	Args json.RawMessage
}

// WithInvocation returns a context that carries the invocation.
func WithInvocation(ctx context.Context, tool string, args json.RawMessage) context.Context {
	return context.WithValue(ctx, invocationKey{}, Invocation{Tool: tool, Args: args})
}

// InvocationFrom returns the invocation of the context, if any.
func InvocationFrom(ctx context.Context) (Invocation, bool) {
	inv, ok := ctx.Value(invocationKey{}).(Invocation)
	return inv, ok
}

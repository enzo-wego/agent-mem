package handlers

import "context"

type fixedScopeBuilder struct {
	scopes []string
	err    error
}

func (b fixedScopeBuilder) For(context.Context, int) ([]string, error) { return b.scopes, b.err }

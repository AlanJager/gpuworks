package auth

import "context"

type claimsKey struct{}

// WithClaims returns ctx carrying the signed-in user's verified claims, for
// handlers mounted below management authentication that need to know who is
// calling (the playground's request records).
func WithClaims(ctx context.Context, c Claims) context.Context {
	return context.WithValue(ctx, claimsKey{}, c)
}

// ClaimsFrom returns the claims WithClaims stored, if any.
func ClaimsFrom(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(claimsKey{}).(Claims)
	return c, ok
}

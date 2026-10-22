package auth

import (
	"context"
	"testing"
)

func TestClaimsRoundTripThroughContext(t *testing.T) {
	if _, ok := ClaimsFrom(context.Background()); ok {
		t.Fatal("claims found in an empty context")
	}
	ctx := WithClaims(context.Background(), Claims{UserID: 9, Username: "ana", Role: "admin"})
	got, ok := ClaimsFrom(ctx)
	if !ok || got.UserID != 9 || got.Username != "ana" {
		t.Fatalf("claims = %+v, %v", got, ok)
	}
}

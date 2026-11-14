package auth

import (
	"context"
	"testing"
)

// A caller that names an unknown kid and goes away must not use up the issuer's refresh: the next honest caller, with a
// context of its own, must still get the key set.
func TestCancelledRequestDoesNotLockOutSignIn(t *testing.T) {
	srv := newJWKSServer(t, jwkRSA("k1", &testRSA.PublicKey, nil))
	o := newOIDC(t, srv)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := o.Authenticate(cancelled, mint(rsHeader("nope"), goodClaims(nil), signRS256(testRSA)))
	mustRefuse(t, err)
	good := mint(rsHeader("k1"), goodClaims(nil), signRS256(testRSA))
	if _, err := o.Authenticate(context.Background(), good); err != nil {
		t.Fatalf("a valid token was refused after a cancelled request: %v", err)
	}
}

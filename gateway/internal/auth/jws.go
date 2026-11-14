package auth

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math"
	"math/big"
	"strings"
	"time"
)

// MaxTokenBytes is the largest bearer token any authenticator here parses. Real ones are 1-3 KB (a Dex ID token, a
// Kubernetes projected token, a GitHub Actions token); the Python verifier has no cap, the Go one refuses anything
// over this before it decodes a byte, so a request cannot make the gateway parse megabytes.
const MaxTokenBytes = 16 << 10

// The only signature algorithms: asymmetric ones, and each tied to its key type. Never "none", never HMAC (an HS256
// token "signed" with a public key), never anything the Python verifier does not list.
const (
	algRS256 = "RS256"
	algES256 = "ES256"
)

// jwt is a compact JWS split into its parts but not yet verified.
type jwt struct {
	alg     string
	kid     string
	hasKid  bool
	signing []byte // header "." payload, the bytes the signature covers
	sig     []byte
	payload []byte
}

// parseJWT splits and decodes a compact JWS. It checks shape only; the signature is not looked at.
func parseJWT(token string) (*jwt, error) {
	if len(token) > MaxTokenBytes {
		return nil, refuse("token rejected")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, refuse("token rejected")
	}
	hb, err1 := b64(parts[0])
	pb, err2 := b64(parts[1])
	sb, err3 := b64(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, refuse("token rejected")
	}
	var h map[string]any
	if json.Unmarshal(hb, &h) != nil || h == nil {
		return nil, refuse("token rejected")
	}
	if _, crit := h["crit"]; crit {
		return nil, refuse("token rejected") // critical extensions this verifier does not implement
	}
	alg, ok := h["alg"].(string)
	if !ok {
		return nil, refuse("token rejected")
	}
	t := &jwt{alg: alg, signing: []byte(parts[0] + "." + parts[1]), sig: sb, payload: pb}
	if k, present := h["kid"]; present && k != nil {
		s, ok := k.(string)
		if !ok {
			return nil, refuse("token rejected")
		}
		t.kid, t.hasKid = s, true
	}
	return t, nil
}

func b64(s string) ([]byte, error) { return base64.RawURLEncoding.Strict().DecodeString(s) }

// claims decodes the payload as a JSON object with numbers kept exact.
func (t *jwt) claims() (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(t.payload))
	dec.UseNumber()
	var c map[string]any
	if dec.Decode(&c) != nil || c == nil || dec.More() {
		return nil, refuse("token rejected")
	}
	return c, nil
}

// verify checks the signature with key, for one of algs only. The header's alg must be in algs AND fit the key's
// type (RS256 with an RSA key, ES256 with a P-256 key); anything else, including a key of the wrong kind, is refused.
func (t *jwt) verify(key crypto.PublicKey, algs []string) error {
	allowed := false
	for _, a := range algs {
		allowed = allowed || a == t.alg
	}
	if !allowed {
		return refuse("token rejected")
	}
	digest := sha256.Sum256(t.signing)
	switch t.alg {
	case algRS256:
		k, ok := key.(*rsa.PublicKey)
		if !ok || rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], t.sig) != nil {
			return refuse("token rejected")
		}
	case algES256:
		k, ok := key.(*ecdsa.PublicKey)
		if !ok || k.Curve != elliptic.P256() || len(t.sig) != 64 {
			return refuse("token rejected")
		}
		r, s := new(big.Int).SetBytes(t.sig[:32]), new(big.Int).SetBytes(t.sig[32:])
		if !ecdsa.Verify(k, digest[:], r, s) {
			return refuse("token rejected")
		}
	default:
		return refuse("token rejected")
	}
	return nil
}

// checkClaims applies PyJWT's rules for decode(audience=, issuer=, leeway=, require=[exp iat iss aud sub]):
//   - every required claim is present and not null;
//   - iat and nbf are not in the future by more than leeway, exp is still ahead (exp <= now-leeway is expired);
//   - iss equals the configured issuer exactly (a string, byte for byte, so a trailing slash differs);
//   - aud (a string, or a list of strings) holds the configured audience exactly;
//   - sub is a string.
//
// Where Python coerces (int("123") accepts a string, a float is truncated), Go is stricter: a time claim is a JSON
// number, and a float is truncated as Python does.
func checkClaims(c map[string]any, issuer, audience string, leeway time.Duration, now time.Time) error {
	for _, k := range []string{"exp", "iat", "iss", "aud", "sub"} {
		if v, ok := c[k]; !ok || v == nil {
			return refuse("token rejected")
		}
	}
	nowS, lee := float64(now.Unix()), leeway.Seconds()
	iat, ok := numericDate(c["iat"])
	if !ok || iat > nowS+lee {
		return refuse("token rejected")
	}
	if v, present := c["nbf"]; present && v != nil {
		nbf, ok := numericDate(v)
		if !ok || nbf > nowS+lee {
			return refuse("token rejected")
		}
	}
	exp, ok := numericDate(c["exp"])
	if !ok || exp <= nowS-lee {
		return refuse("token rejected")
	}
	if iss, ok := c["iss"].(string); !ok || iss != issuer {
		return refuse("token rejected")
	}
	if !audienceMatches(c["aud"], audience) {
		return refuse("token rejected")
	}
	if _, ok := c["sub"].(string); !ok {
		return refuse("token rejected")
	}
	return nil
}

// numericDate reads a JSON number as whole seconds, truncated toward zero as Python's int() does.
func numericDate(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return math.Trunc(f), true
}

func audienceMatches(aud any, want string) bool {
	switch a := aud.(type) {
	case string:
		return a != "" && a == want
	case []any:
		found := false
		for _, e := range a {
			s, ok := e.(string)
			if !ok {
				return false // a list with a non-string is malformed, as in PyJWT
			}
			found = found || s == want
		}
		return found
	}
	return false
}

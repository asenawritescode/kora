package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/asenawritescode/kora/auth/oidc"
)

const jwksRefreshInterval = time.Minute

type cachedJWKS struct {
	keys      map[string]any
	fetchedAt time.Time
}

type jsonWebKey struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Use string `json:"use"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// signingKey returns the issuer's public key for kid, refetching the key set
// once per minute at most when kid is unknown (key rotation).
func (l *OIDCLogin) signingKey(ctx context.Context, issuer, jwksURI, kid string) (any, error) {
	l.mu.Lock()
	cached, ok := l.jwks[issuer]
	l.mu.Unlock()
	if ok {
		if key, found := cached.keys[kid]; found {
			return key, nil
		}
		if l.now().Sub(cached.fetchedAt) < jwksRefreshInterval {
			return nil, oidc.ErrKeyNotFound
		}
	}
	var set struct {
		Keys []jsonWebKey `json:"keys"`
	}
	if err := l.getJSON(ctx, jwksURI, &set); err != nil {
		return nil, fmt.Errorf("fetch signing keys: %w", err)
	}
	keys := make(map[string]any, len(set.Keys))
	for _, jwk := range set.Keys {
		if jwk.Use != "" && jwk.Use != "sig" {
			continue
		}
		if key, err := jwk.publicKey(); err == nil {
			keys[jwk.Kid] = key
		}
	}
	l.mu.Lock()
	if l.jwks == nil {
		l.jwks = make(map[string]cachedJWKS)
	}
	l.jwks[issuer] = cachedJWKS{keys: keys, fetchedAt: l.now()}
	l.mu.Unlock()
	key, found := keys[kid]
	if !found {
		return nil, oidc.ErrKeyNotFound
	}
	return key, nil
}

func (k jsonWebKey) publicKey() (any, error) {
	switch k.Kty {
	case "RSA":
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, err
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			return nil, errors.New("invalid RSA exponent")
		}
		key := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if key.N.BitLen() < 2048 {
			return nil, errors.New("RSA key shorter than 2048 bits")
		}
		return key, nil
	case "EC":
		if k.Crv != "P-256" {
			return nil, fmt.Errorf("unsupported curve %q", k.Crv)
		}
		x, errX := base64.RawURLEncoding.DecodeString(k.X)
		y, errY := base64.RawURLEncoding.DecodeString(k.Y)
		if errX != nil || errY != nil {
			return nil, errors.New("invalid EC coordinates")
		}
		key := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !key.Curve.IsOnCurve(key.X, key.Y) {
			return nil, errors.New("EC point not on curve")
		}
		return key, nil
	default:
		return nil, fmt.Errorf("unsupported key type %q", k.Kty)
	}
}

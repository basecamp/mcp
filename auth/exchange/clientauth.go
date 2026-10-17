package exchange

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	clientAssertionTypeJWTBearer = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" // #nosec G101 -- RFC 7523 URN, not a credential

	// assertionLifetime bounds each private_key_jwt client assertion. Short
	// on purpose: an assertion is minted per request and single-use (fresh
	// jti), so a stolen one must expire before it is worth replaying.
	assertionLifetime = 60 * time.Second
)

// ClientAuthenticator applies this server's client authentication to a
// token request. Called once per request (private_key_jwt mints a fresh
// assertion each call).
type ClientAuthenticator interface {
	Authenticate(form url.Values) error
}

// ClientSecretPost authenticates with client_id + client_secret form
// fields (OAuth 2.0 client_secret_post).
func ClientSecretPost(clientID, secret string) ClientAuthenticator {
	return &clientSecretPost{clientID: clientID, secret: secret}
}

type clientSecretPost struct {
	clientID string
	secret   string
}

func (c *clientSecretPost) Authenticate(form url.Values) error {
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.secret)
	return nil
}

// PrivateKeyJWT authenticates with an RFC 7523 §2.2 client assertion
// signed ES256. The signer must be an *ecdsa.PrivateKey on P-256 - the
// only algorithm this deployment registers, so any other key is a wiring
// mistake to reject at construction, not a request-time surprise.
func PrivateKeyJWT(clientID string, key crypto.Signer, tokenURL string) (ClientAuthenticator, error) {
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private_key_jwt requires an *ecdsa.PrivateKey, got %T", key)
	}
	if ec.Curve != elliptic.P256() {
		return nil, fmt.Errorf("private_key_jwt requires a P-256 key, got %s", ec.Curve.Params().Name)
	}

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: ec}, nil)
	if err != nil {
		return nil, fmt.Errorf("constructing ES256 signer: %w", err)
	}

	return &privateKeyJWT{clientID: clientID, tokenURL: tokenURL, signer: signer}, nil
}

// LoadPrivateKeyJWTFromPEM reads a P-256 private key from a PEM file (EC
// PRIVATE KEY or PKCS#8) and wraps it in PrivateKeyJWT.
func LoadPrivateKeyJWTFromPEM(clientID, pemPath, tokenURL string) (ClientAuthenticator, error) {
	data, err := os.ReadFile(pemPath)
	if err != nil {
		return nil, fmt.Errorf("reading private key: %w", err)
	}
	key, err := parseECPrivateKeyPEM(data)
	if err != nil {
		return nil, fmt.Errorf("parsing private key from %s: %w", pemPath, err)
	}
	return PrivateKeyJWT(clientID, key, tokenURL)
}

// parseECPrivateKeyPEM accepts the two encodings a P-256 key is deployed
// as. Curve enforcement lives in PrivateKeyJWT so both entry points share
// it.
func parseECPrivateKeyPEM(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	switch block.Type {
	case "EC PRIVATE KEY":
		return x509.ParseECPrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		ec, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS#8 key is %T, need *ecdsa.PrivateKey", key)
		}
		return ec, nil
	default:
		return nil, fmt.Errorf("unsupported PEM block type %q", block.Type)
	}
}

type privateKeyJWT struct {
	clientID string
	tokenURL string
	signer   jose.Signer
}

func (p *privateKeyJWT) Authenticate(form url.Values) error {
	jti, err := randomJTI()
	if err != nil {
		return err
	}

	now := time.Now()
	claims := jwt.Claims{
		Issuer:   p.clientID,
		Subject:  p.clientID,
		Audience: jwt.Audience{p.tokenURL},
		IssuedAt: jwt.NewNumericDate(now),
		Expiry:   jwt.NewNumericDate(now.Add(assertionLifetime)),
		ID:       jti,
	}
	assertion, err := jwt.Signed(p.signer).Claims(claims).Serialize()
	if err != nil {
		return fmt.Errorf("signing client assertion: %w", err)
	}

	form.Set("client_id", p.clientID)
	form.Set("client_assertion_type", clientAssertionTypeJWTBearer)
	form.Set("client_assertion", assertion)
	return nil
}

// randomJTI mints a unique assertion ID so the AS can enforce single use.
func randomJTI() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand failed: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Package handshake wraps Noise_IK_25519_ChaChaPoly_BLAKE2s, the pattern
// WireGuard uses: the initiator (a client router) knows the responder's static
// key, the responder learns the initiator's from the first message and checks
// it against its peers. Each completed handshake yields one AEAD per direction,
// used with explicit nonces because packets arrive out of order across paths.
package handshake

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/flynn/noise"
	"golang.org/x/crypto/curve25519"
)

// Key is a Curve25519 key, private or public.
type Key [32]byte

// String encodes the key as standard base64, like WireGuard keys.
func (k Key) String() string { return base64.StdEncoding.EncodeToString(k[:]) }

// IsZero reports whether the key is unset.
func (k Key) IsZero() bool { return k == Key{} }

// ParseKey decodes a base64 key.
func ParseKey(s string) (Key, error) {
	var k Key
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return k, fmt.Errorf("key is not base64: %w", err)
	}
	if len(b) != len(k) {
		return k, fmt.Errorf("key is %d bytes, want 32", len(b))
	}
	copy(k[:], b)
	return k, nil
}

// GenerateKey makes a new clamped private key.
func GenerateKey() (Key, error) {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		return k, err
	}
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	return k, nil
}

// PublicKey derives the public key for a private key.
func PublicKey(priv Key) Key {
	var pub Key
	out, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		// Only a low-order result errors, which a clamped private key can't give.
		panic(err)
	}
	copy(pub[:], out)
	return pub
}

// Cipher is one direction of a completed handshake: an AEAD keyed for that
// direction, used with explicit 64-bit nonces. It is safe for concurrent use.
type Cipher = noise.Cipher

var suite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)

var prologue = []byte("weaver v1")

func dhKey(priv Key) noise.DHKey {
	pub := PublicKey(priv)
	return noise.DHKey{Private: append([]byte(nil), priv[:]...), Public: pub[:]}
}

// Initiator is a handshake in progress on the client side.
type Initiator struct {
	hs *noise.HandshakeState
}

// Initiate starts a handshake to the responder with static key remote and
// returns the first message, carrying payload.
func Initiate(local, remote Key, payload []byte) (*Initiator, []byte, error) {
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:   suite,
		Random:        rand.Reader,
		Pattern:       noise.HandshakeIK,
		Initiator:     true,
		Prologue:      prologue,
		StaticKeypair: dhKey(local),
		PeerStatic:    remote[:],
	})
	if err != nil {
		return nil, nil, err
	}
	msg, _, _, err := hs.WriteMessage(nil, payload)
	if err != nil {
		return nil, nil, err
	}
	return &Initiator{hs: hs}, msg, nil
}

// Finish reads the responder's message and returns the send and receive
// ciphers and the responder's payload.
func (i *Initiator) Finish(msg []byte) (send, recv Cipher, payload []byte, err error) {
	payload, cs1, cs2, err := i.hs.ReadMessage(nil, msg)
	if err != nil {
		return nil, nil, nil, err
	}
	if cs1 == nil || cs2 == nil {
		return nil, nil, nil, errors.New("handshake: not complete after response")
	}
	return cs1.Cipher(), cs2.Cipher(), payload, nil
}

// Response is the responder's side of a completed handshake.
type Response struct {
	Peer    Key    // the initiator's static key
	Payload []byte // the initiator's payload
	Message []byte // the message to send back
	Send    Cipher
	Recv    Cipher
}

// ErrUnknownPeer is returned when the initiator's static key is not allowed.
var ErrUnknownPeer = errors.New("handshake: unknown peer")

// Respond reads an initiator's first message. allowed decides whether the
// initiator's static key belongs to a peer; if it does, Respond answers with
// reply as the payload.
func Respond(local Key, msg []byte, allowed func(Key) bool, reply func(peer Key, payload []byte) []byte) (*Response, error) {
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:   suite,
		Random:        rand.Reader,
		Pattern:       noise.HandshakeIK,
		Initiator:     false,
		Prologue:      prologue,
		StaticKeypair: dhKey(local),
	})
	if err != nil {
		return nil, err
	}
	payload, _, _, err := hs.ReadMessage(nil, msg)
	if err != nil {
		return nil, err
	}
	var peer Key
	copy(peer[:], hs.PeerStatic())
	if !allowed(peer) {
		return nil, ErrUnknownPeer
	}
	out, cs1, cs2, err := hs.WriteMessage(nil, reply(peer, payload))
	if err != nil {
		return nil, err
	}
	if cs1 == nil || cs2 == nil {
		return nil, errors.New("handshake: not complete after response")
	}
	return &Response{
		Peer:    peer,
		Payload: payload,
		Message: out,
		Send:    cs2.Cipher(),
		Recv:    cs1.Cipher(),
	}, nil
}

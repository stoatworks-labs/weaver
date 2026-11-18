package handshake

import (
	"bytes"
	"testing"
)

func TestHandshakeOutOfOrderNonces(t *testing.T) {
	cPriv, _ := GenerateKey()
	sPriv, _ := GenerateKey()
	cPub, sPub := PublicKey(cPriv), PublicKey(sPriv)

	ini, msg1, err := Initiate(cPriv, sPub, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Respond(sPriv, msg1,
		func(k Key) bool { return k == cPub },
		func(_ Key, payload []byte) []byte { return append([]byte("re:"), payload...) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Peer != cPub || string(res.Payload) != "hello" {
		t.Fatalf("responder saw %v %q", res.Peer, res.Payload)
	}
	cSend, cRecv, payload, err := ini.Finish(res.Message)
	if err != nil || string(payload) != "re:hello" {
		t.Fatalf("finish: %q %v", payload, err)
	}

	ad := []byte("header")
	// Client to server, nonces out of order.
	for _, n := range []uint64{5, 1, 1 << 60, 3} {
		ct := cSend.Encrypt(nil, n, ad, []byte("ping"))
		pt, err := res.Recv.Decrypt(nil, n, ad, ct)
		if err != nil || string(pt) != "ping" {
			t.Fatalf("nonce %d: %q %v", n, pt, err)
		}
	}
	// Server to client.
	ct := res.Send.Encrypt(nil, 9, ad, []byte("pong"))
	if pt, err := cRecv.Decrypt(nil, 9, ad, ct); err != nil || string(pt) != "pong" {
		t.Fatalf("pong: %q %v", pt, err)
	}
	// Tampered header fails.
	ct = cSend.Encrypt(nil, 10, ad, []byte("x"))
	if _, err := res.Recv.Decrypt(nil, 10, []byte("headex"), ct); err == nil {
		t.Fatal("tampered associated data decrypted")
	}
	// The two directions use different keys.
	ct = cSend.Encrypt(nil, 11, ad, []byte("y"))
	if _, err := cRecv.Decrypt(nil, 11, ad, ct); err == nil {
		t.Fatal("client could decrypt its own direction")
	}
}

func TestUnknownPeerRejected(t *testing.T) {
	cPriv, _ := GenerateKey()
	sPriv, _ := GenerateKey()
	_, msg1, _ := Initiate(cPriv, PublicKey(sPriv), nil)
	_, err := Respond(sPriv, msg1, func(Key) bool { return false }, func(Key, []byte) []byte { return nil })
	if err != ErrUnknownPeer {
		t.Fatalf("got %v", err)
	}
}

func TestWrongServerKeyFails(t *testing.T) {
	cPriv, _ := GenerateKey()
	sPriv, _ := GenerateKey()
	other, _ := GenerateKey()
	_, msg1, _ := Initiate(cPriv, PublicKey(other), nil)
	if _, err := Respond(sPriv, msg1, func(Key) bool { return true }, func(Key, []byte) []byte { return nil }); err == nil {
		t.Fatal("a server answered an init meant for another key")
	}
}

func TestKeyEncoding(t *testing.T) {
	k, _ := GenerateKey()
	back, err := ParseKey(k.String())
	if err != nil || !bytes.Equal(back[:], k[:]) {
		t.Fatal("round trip failed")
	}
	if _, err := ParseKey("short"); err == nil {
		t.Fatal("bad key parsed")
	}
}

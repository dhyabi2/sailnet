// Package relay implements the Sailnet relay (sailnode) and circuit client.
package relay

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"

	"github.com/dhyabi2/sail/nano"
	"github.com/dhyabi2/sail/wire"
	"golang.org/x/crypto/blake2b"
)

// Descriptor is the on-ledger endpoint record carried in DESCRIPTOR aux:
// ipv4[4] ‖ port[2] ‖ certFP[6] (first 6 bytes of sha256 of the TLS leaf cert).
type Descriptor struct {
	IP     net.IP
	Port   uint16
	CertFP [6]byte
}

// Encode packs the descriptor into 12 bytes.
func (d Descriptor) Encode() [12]byte {
	var a [12]byte
	copy(a[0:4], d.IP.To4())
	binary.BigEndian.PutUint16(a[4:6], d.Port)
	copy(a[6:12], d.CertFP[:])
	return a
}

// DecodeDescriptor parses 12 bytes; ok=false if empty.
func DecodeDescriptor(a [12]byte) (Descriptor, bool) {
	if a == ([12]byte{}) {
		return Descriptor{}, false
	}
	d := Descriptor{IP: net.IPv4(a[0], a[1], a[2], a[3]), Port: binary.BigEndian.Uint16(a[4:6])}
	copy(d.CertFP[:], a[6:12])
	return d, true
}

func (d Descriptor) Addr() string { return fmt.Sprintf("%s:%d", d.IP, d.Port) }

// DayNumber is the UTC day used in access tokens.
func DayNumber(t time.Time) uint32 { return uint32(t.UTC().Unix() / 86400) }

// AccessToken derives the tunnel token: blake2b(relayPub ‖ day ‖ nonce)[:16].
// Anyone who can read the ledger can compute it; its purpose is to make the
// relay indistinguishable from a website to anyone who cannot (active probers
// that do not hold the relay's on-ledger key), not to be a secret.
// A bridge adds a secret from its bridge line, so a censor who reads the
// ledger (or gets the IP) still cannot confirm the relay by probing: without
// the secret every request gets the decoy site.
func AccessToken(relayPub [32]byte, secret [16]byte, day uint32, nonce [8]byte) string {
	h, _ := blake2b.New(16, nil)
	h.Write(relayPub[:])
	h.Write(secret[:])
	var d [4]byte
	binary.BigEndian.PutUint32(d[:], day)
	h.Write(d[:])
	h.Write(nonce[:])
	return hex.EncodeToString(h.Sum(nil))
}

// TunnelPath builds the URL path used to open a tunnel.
func TunnelPath(relayPub [32]byte, secret [16]byte, now time.Time) (path string, nonce [8]byte) {
	rand.Read(nonce[:])
	return "/t/" + hex.EncodeToString(nonce[:]) + "/" + AccessToken(relayPub, secret, DayNumber(now), nonce), nonce
}

// CheckTunnelPath validates a path against today or yesterday (clock skew).
func CheckTunnelPath(relayPub [32]byte, secret [16]byte, path string, now time.Time) bool {
	var nonceHex, tok string
	if n, _ := fmt.Sscanf(path, "/t/%16s/%32s", &nonceHex, &tok); n != 2 {
		return false
	}
	nb, err := hex.DecodeString(nonceHex)
	if err != nil || len(nb) != 8 {
		return false
	}
	var nonce [8]byte
	copy(nonce[:], nb)
	day := DayNumber(now)
	return tok == AccessToken(relayPub, secret, day, nonce) || tok == AccessToken(relayPub, secret, day-1, nonce)
}

// SelfSignedCert makes a TLS cert that looks like an ordinary small site.
func SelfSignedCert(host string) (tls.Certificate, [6]byte, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, [6]byte{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(825 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, [6]byte{}, err
	}
	keyDER, _ := x509.MarshalECPrivateKey(priv)
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	var fp [6]byte
	s := sha256.Sum256(der)
	copy(fp[:], s[:6])
	return cert, fp, err
}

// CertFP6 returns the 6-byte fingerprint of a leaf cert.
func CertFP6(der []byte) [6]byte {
	var fp [6]byte
	s := sha256.Sum256(der)
	copy(fp[:], s[:6])
	return fp
}

// SignAck signs the CREATED ack: sig over "sailnet-ack" ‖ clientPub ‖ hopPub.
// The ack also commits to the relay's TLS leaf certificate (SHA-256), so a
// box in the middle that forged the short fingerprint pin and terminated TLS
// is caught: the certificate the client saw is not the one the ledger key
// signed for.
func SignAck(k *nano.Key, clientPub, hopPub, certHash [32]byte) []byte {
	h, _ := blake2b.New256(nil)
	h.Write([]byte("sailnet-ack2"))
	h.Write(clientPub[:])
	h.Write(hopPub[:])
	h.Write(certHash[:])
	return k.Sign(h.Sum(nil))
}

// VerifyAck checks the CREATED signature against the relay's on-ledger key.
func VerifyAck(relayPub, clientPub, hopPub, certHash [32]byte, sig []byte) bool {
	h, _ := blake2b.New256(nil)
	h.Write([]byte("sailnet-ack2"))
	h.Write(clientPub[:])
	h.Write(hopPub[:])
	h.Write(certHash[:])
	return nano.Verify(relayPub, h.Sum(nil), sig)
}

// HomeHello builds the HOME_HELLO cell a home node sends over its outbound
// tunnel: account(65) ‖ sig[64] over "sailnet-home" ‖ harbourPub ‖ poolTag ‖ poolTag[32].
func HomeHello(k *nano.Key, harbourPub [32]byte, poolTagHex string) []byte {
	tag, _ := hex.DecodeString(poolTagHex)
	var t [32]byte
	copy(t[:], tag)
	h, _ := blake2b.New256(nil)
	h.Write([]byte("sailnet-home"))
	h.Write(harbourPub[:])
	h.Write(t[:])
	sig := k.Sign(h.Sum(nil))
	payload := append([]byte(k.Address), sig...)
	payload = append(payload, t[:]...)
	c := &wire.Cell{CircID: 0, Cmd: wire.CmdHomeHello, Payload: payload}
	return c.Marshal()
}

// SignCreate binds a CREATE to the payer: sig over "sailnet-create" ‖ clientPub ‖ tag
// by the key that signed the payment block. A tag copied off the public ledger
// is useless without this signature.
func SignCreate(k *nano.Key, clientPub, tag [32]byte) []byte {
	h, _ := blake2b.New256(nil)
	h.Write([]byte("sailnet-create"))
	h.Write(clientPub[:])
	h.Write(tag[:])
	return k.Sign(h.Sum(nil))
}

// VerifyCreate checks a CREATE signature against the tag owner's public key.
func VerifyCreate(ownerPub [32]byte, clientPub, tag [32]byte, sig []byte) bool {
	h, _ := blake2b.New256(nil)
	h.Write([]byte("sailnet-create"))
	h.Write(clientPub[:])
	h.Write(tag[:])
	return nano.Verify(ownerPub, h.Sum(nil), sig)
}

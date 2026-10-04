package trae

// tc container decryption.
//
// Ported from the MIT-licensed reference client2api-lab/_upstream/trae2api
// (src/trae-decrypt.js, wangqi233).  The Trae desktop app stores its auth blob
// in User/globalStorage/storage.json under the key
// "iCubeAuthInfo://icube.cloudide" as base64 of a "tc" container:
//
//	[6-byte header][32-byte random][AES-128-CBC ciphertext]
//
// The ciphertext decrypts (after PKCS#7 unpadding) to
//
//	[64-byte sha512(plaintext)][plaintext]
//
// and the trailing 64 bytes must match sha512 of the plaintext, which is the
// integrity check that makes a wrong key fail loudly instead of silently
// producing garbage.
//
// Key derivation is sha512(sha512(random) || salt), truncated to a 16-byte AES
// key and a 16-byte IV.  The salt is a fixed XOR of two 64-byte tables; the
// tables differ between the "AES" and "AES_PRIVATE" container flavours.  The
// reference concatenates the inner digest with the salt (the prose summary in
// docs/upstream/trae.md says "XOR"; the JavaScript is authoritative and is what
// the real desktop app matches).
//
// NOTE: this is not a general-purpose cipher utility.  It exists to read the
// credential file that the Trae desktop app already wrote on this machine.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
)

// Container geometry.
const (
	tcHeaderSize = 6
	tcRandomLen  = 32
	tcHashLen    = 64
	tcKeyLen     = 16
	tcIVLen      = 16
	tcMinLen     = tcHeaderSize + tcRandomLen + tcHashLen + tcIVLen
)

// Container flavours, identified by the 6-byte magic header.
const (
	encUnknown = iota
	encAES
	encAESPrivate
)

// The four 64-byte salt tables.  They are public constants of the Trae client;
// the reference exposes them as TRAE_SALT_A..D overrides, which this port does
// not need (see README "known gaps").
var (
	tcSaltA = []byte{
		82, 9, 106, 213, 48, 54, 165, 56, 191, 64, 163, 158, 129, 243, 215, 251,
		124, 227, 57, 130, 155, 47, 255, 135, 52, 142, 67, 68, 196, 222, 233, 203,
		84, 123, 148, 50, 166, 194, 35, 61, 238, 76, 149, 11, 66, 250, 195, 78,
		8, 46, 161, 102, 40, 217, 36, 178, 118, 91, 162, 73, 109, 139, 209, 37,
	}
	tcSaltB = []byte{
		31, 221, 168, 51, 136, 7, 199, 49, 177, 18, 16, 89, 39, 128, 236, 95,
		96, 81, 127, 169, 25, 181, 74, 13, 45, 229, 122, 159, 147, 201, 156, 239,
		160, 224, 59, 77, 174, 42, 245, 176, 200, 235, 187, 60, 131, 83, 153, 97,
		23, 43, 4, 126, 186, 119, 214, 38, 225, 105, 20, 99, 85, 33, 12, 125,
	}
	tcSaltC = []byte{
		191, 192, 216, 250, 122, 246, 220, 97, 31, 254, 98, 27, 8, 72, 71, 176,
		135, 99, 96, 18, 127, 101, 203, 104, 211, 102, 191, 125, 37, 72, 150, 156,
		51, 229, 121, 35, 17, 153, 141, 177, 110, 131, 150, 128, 172, 255, 254, 6,
		18, 140, 55, 62, 236, 249, 135, 64, 135, 12, 117, 4, 89, 149, 168, 209,
	}
	tcSaltD = []byte{
		246, 204, 26, 232, 232, 70, 129, 109, 223, 146, 169, 242, 23, 241, 105, 145,
		50, 196, 165, 42, 254, 120, 3, 54, 244, 207, 209, 85, 53, 6, 138, 106,
		175, 148, 31, 204, 186, 186, 165, 182, 87, 142, 49, 10, 39, 110, 26, 154,
		86, 56, 173, 125, 18, 64, 198, 225, 99, 99, 83, 82, 191, 134, 76, 170,
	}
)

var (
	// ErrNotTCContainer means the buffer is not a tc container at all (no "tc"
	// magic, or too short).
	ErrNotTCContainer = errors.New("not a tc container")
	// ErrTCHashMismatch means the container decrypted but its integrity hash did
	// not match: wrong key, corrupted data, or a container flavour this port does
	// not know.
	ErrTCHashMismatch = errors.New("tc container hash verification failed")
)

// IsTCContainer reports whether buf starts with the "tc" magic.
func IsTCContainer(buf []byte) bool {
	return len(buf) >= 2 && buf[0] == 0x74 && buf[1] == 0x63
}

// detectEncType classifies a container header.
func detectEncType(header []byte) int {
	if len(header) < tcHeaderSize {
		return encUnknown
	}
	if header[0] == 0x74 && header[1] == 0x63 && header[2] == 0x05 &&
		header[3] == 0x10 && header[4] == 0x00 && header[5] == 0x00 {
		return encAES
	}
	if header[0] == 0x12 && header[1] == 0x57 && header[2] == 0x20 &&
		header[3] == 0x20 && header[4] == 0x02 && header[5] == 0x03 {
		return encAESPrivate
	}
	return encUnknown
}

// xorSalts returns a XOR b, byte-wise.
func xorSalts(a, b []byte) []byte {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// deriveKeyAndIV reproduces the reference key schedule.
func deriveKeyAndIV(randomBytes []byte, encType int) (key, iv []byte, err error) {
	if len(randomBytes) != tcRandomLen {
		return nil, nil, fmt.Errorf("tc: random block is %d bytes, want %d", len(randomBytes), tcRandomLen)
	}
	var salt []byte
	switch encType {
	case encAESPrivate:
		salt = xorSalts(tcSaltC, tcSaltD)
	default:
		salt = xorSalts(tcSaltA, tcSaltB)
	}
	inner := sha512.Sum512(randomBytes)
	// sha512(inner || salt)
	h := sha512.New()
	h.Write(inner[:])
	h.Write(salt)
	final := h.Sum(nil)
	return final[0:tcKeyLen], final[tcKeyLen : tcKeyLen+tcIVLen], nil
}

// unpadPKCS7 strips PKCS#7 padding.  Node's createDecipheriv removes it by
// default, so the reference relies on this being implicit; Go's CBC mode does
// not.
func unpadPKCS7(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("tc: empty plaintext")
	}
	pad := int(b[len(b)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(b) {
		return nil, errors.New("tc: invalid padding")
	}
	for i := len(b) - pad; i < len(b); i++ {
		if int(b[i]) != pad {
			return nil, errors.New("tc: invalid padding")
		}
	}
	return b[:len(b)-pad], nil
}

// verifyAndStrip splits a decrypted buffer into its stored hash and payload and
// checks the hash.  It tolerates containers whose payload is not padded.
func verifyAndStrip(decrypted []byte) ([]byte, bool) {
	if len(decrypted) < tcHashLen {
		return nil, false
	}
	stored := decrypted[0:tcHashLen]
	candidates := make([][]byte, 0, 2)
	if unpadded, err := unpadPKCS7(decrypted); err == nil {
		candidates = append(candidates, unpadded)
	}
	candidates = append(candidates, decrypted)
	for _, cand := range candidates {
		if len(cand) < tcHashLen {
			continue
		}
		payload := cand[tcHashLen:]
		sum := sha512.Sum512(payload)
		if len(cand) >= tcHashLen && string(sum[:]) == string(stored) {
			return payload, true
		}
	}
	return nil, false
}

// DecryptTC decrypts a tc container and returns its plaintext bytes.
func DecryptTC(buf []byte) ([]byte, error) {
	if !IsTCContainer(buf) {
		return nil, ErrNotTCContainer
	}
	if len(buf) < tcMinLen {
		return nil, fmt.Errorf("%w: buffer is %d bytes, need at least %d", ErrNotTCContainer, len(buf), tcMinLen)
	}
	header := buf[0:tcHeaderSize]
	randomBytes := buf[tcHeaderSize : tcHeaderSize+tcRandomLen]
	encrypted := buf[tcHeaderSize+tcRandomLen:]

	encType := detectEncType(header)
	if encType == encUnknown {
		return nil, fmt.Errorf("%w: unknown header %x", ErrNotTCContainer, header)
	}
	key, iv, err := deriveKeyAndIV(randomBytes, encType)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(encrypted)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("tc: ciphertext length %d is not a multiple of the AES block size", len(encrypted))
	}
	decrypted := make([]byte, len(encrypted))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(decrypted, encrypted)

	plaintext, ok := verifyAndStrip(decrypted)
	if !ok {
		return nil, ErrTCHashMismatch
	}
	return plaintext, nil
}

// DecryptTCBase64 base64-decodes a stored value and decrypts it.
func DecryptTCBase64(value string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		// Some writers use the URL-safe alphabet.
		raw, err = base64.RawURLEncoding.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("tc: base64 decode: %w", err)
		}
	}
	return DecryptTC(raw)
}

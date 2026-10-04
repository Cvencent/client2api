package zcode

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"os/user"
	"runtime"
	"strings"
)

// The ZCode desktop client stores part of its own state with an envelope cipher
// it names "enc:v1:".  One of those values is the credential this module
// actually needs: the coding-plan API key, which the client writes to
// credentials.json as "id.secret" and keeps encrypted, while the copy it puts
// in config.json is truncated to the id half.
//
// The envelope is AES-256-GCM under a key the client derives from a
// machine-local passphrase -- SHA-256 of the passphrase, no salt, no KDF
// parameters to guess:
//
//	enc:v1:<b64url(nonce12)>.<b64url(tag16)>.<b64url(ciphertext)>
//
// The passphrase is the environment override when set, otherwise a string
// built from the machine identity, which is why the same value opens on the
// machine that wrote it and nowhere else.
//
// This is not an attack on the cipher and not a bypass: the client's own
// credential is being read on the machine the operator already controls, which
// is exactly what the panel's credential discovery is for.  A value written
// under a different passphrase simply fails to open and is reported as
// unreadable, never guessed at.
const (
	credentialSecretEnv = "ZCODE_CREDENTIAL_SECRET"
	credentialSecretTag = "zcode-credential-fallback:"

	// credentialNonceLen is the fixed GCM nonce the envelope uses.
	credentialNonceLen = 12
	// credentialTagLen is the fixed GCM tag length.
	credentialTagLen = 16
)

// credentialPassphrase is the string the desktop client feeds to SHA-256 to get
// its envelope key.  It mirrors the client's own resolution order so a machine
// that was set up normally needs no configuration at all.
func credentialPassphrase() string {
	if v := strings.TrimSpace(os.Getenv(credentialSecretEnv)); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return credentialSecretTag + credentialPlatform() + ":" + home + ":" + credentialUser()
}

// credentialPlatform renders the client's platform token.
func credentialPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	default:
		return runtime.GOOS
	}
}

// credentialUser renders the client's user token, following the same fallback
// chain (USERNAME on Windows, then USER, then LOGNAME, then the OS record).
func credentialUser() string {
	for _, env := range []string{"USERNAME", "USER", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	if u, err := user.Current(); err == nil {
		name := strings.TrimSpace(u.Username)
		if i := strings.LastIndexAny(name, `\/`); i >= 0 {
			name = name[i+1:]
		}
		if name != "" {
			return name
		}
	}
	return "unknown"
}

// decryptValue opens one "enc:v1:" envelope.  It returns false for anything it
// cannot open -- a different passphrase, a malformed body, a truncated value --
// and never panics: discovery must degrade to "unreadable", not to a failure.
//
// Both observed body layouts are accepted: "nonce.tag.ciphertext" (the client's
// own three-part form) and "nonce.ciphertext+tag" (the two-part form, where the
// GCM tag trails the ciphertext).
func decryptValue(value string) (string, bool) {
	body, ok := strings.CutPrefix(strings.TrimSpace(value), encryptedValuePrefix)
	if !ok {
		return "", false
	}
	parts := strings.Split(body, ".")
	if len(parts) != 2 && len(parts) != 3 {
		return "", false
	}
	nonce, err := decodeBase64URL(parts[0])
	if err != nil || len(nonce) != credentialNonceLen {
		return "", false
	}

	var sealed []byte
	switch len(parts) {
	case 2:
		if sealed, err = decodeBase64URL(parts[1]); err != nil {
			return "", false
		}
	case 3:
		tag, errTag := decodeBase64URL(parts[1])
		ct, errCT := decodeBase64URL(parts[2])
		if errTag != nil || errCT != nil || len(tag) != credentialTagLen {
			return "", false
		}
		sealed = make([]byte, 0, len(ct)+len(tag))
		sealed = append(sealed, ct...)
		sealed = append(sealed, tag...)
	}
	if len(sealed) < credentialTagLen {
		return "", false
	}

	passphrase := credentialPassphrase()
	if passphrase == "" {
		return "", false
	}
	key := sha256.Sum256([]byte(passphrase))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", false
	}
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", false
	}
	return string(plain), true
}

// decodeBase64URL accepts both the unpadded and the padded URL alphabet, since
// the client writes unpadded but a hand-edited value may not be.
func decodeBase64URL(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// openCredential returns a stored value in the clear: an "enc:v1:" envelope is
// decrypted, anything else is already plain.  Empty and unopenable values both
// report false, so a caller cannot accidentally treat "" as a credential.
func openCredential(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	if !strings.HasPrefix(value, encryptedValuePrefix) {
		return value, true
	}
	plain, ok := decryptValue(value)
	if !ok {
		return "", false
	}
	plain = strings.TrimSpace(plain)
	if plain == "" {
		return "", false
	}
	return plain, true
}

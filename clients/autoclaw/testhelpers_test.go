package autoclaw

import (
	"crypto/aes"
	"crypto/cipher"
)

func sealTestBlob(plain, key []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	out := append([]byte("v10"), nonce...)
	return gcm.Seal(out, nonce, plain, nil)
}

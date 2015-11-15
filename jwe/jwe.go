// Package jwe implements JWE as described in https://tools.ietf.org/html/rfc7516
package jwe

import (
	"bytes"
	"compress/flate"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lestrrat/go-jwx/buffer"
	"github.com/lestrrat/go-jwx/internal/debug"
	"github.com/lestrrat/go-jwx/jwa"
)

func Encrypt(payload []byte, keyalg jwa.KeyEncryptionAlgorithm, key interface{}, contentalg jwa.ContentEncryptionAlgorithm, compressalg jwa.CompressionAlgorithm) ([]byte, error) {
	var keyenc KeyEncrypter
	switch keyalg {
	case jwa.RSA1_5, jwa.RSA_OAEP, jwa.RSA_OAEP_256:
		pubkey, ok := key.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("invalid key: *rsa.PublicKey required")
		}
		keyenc = NewRSAKeyEncrypt(keyalg, pubkey)
	case jwa.A128KW, jwa.A192KW, jwa.A256KW:
		sharedkey, ok := key.([]byte)
		if !ok {
			return nil, errors.New("invalid key: []byte required")
		}
		kwenc, err := NewAesKeyWrap(keyalg, sharedkey)
		if err != nil {
			return nil, err
		}
		keyenc = kwenc
	case jwa.ECDH_ES, jwa.ECDH_ES_A128KW, jwa.ECDH_ES_A192KW, jwa.ECDH_ES_A256KW:
		fallthrough
	case jwa.A128GCMKW, jwa.A192GCMKW, jwa.A256GCMKW:
		fallthrough
	case jwa.PBES2_HS256_A128KW, jwa.PBES2_HS384_A192KW, jwa.PBES2_HS512_A256KW:
		fallthrough
	default:
		return nil, ErrUnsupportedAlgorithm
	}

	contentcrypt, err := NewAesCrypt(contentalg)
	if err != nil {
		return nil, err
	}
	enc := NewMultiEncrypt(contentcrypt, NewRandomKeyGenerate(contentcrypt.KeySize()), keyenc)
	msg, err := enc.Encrypt(payload)
	if err != nil {
		return nil, err
	}

	return CompactSerialize{}.Serialize(msg)
}

func Parse(buf []byte) (*Message, error) {
	buf = bytes.TrimSpace(buf)
	if len(buf) == 0 {
		return nil, errors.New("empty buffer")
	}

	if buf[0] == '{' {
		return parseJSON(buf)
	}
	return parseCompact(buf)
}

func ParseString(s string) (*Message, error) {
	return Parse([]byte(s))
}

func parseJSON(buf []byte) (*Message, error) {
	m := struct {
		*Message
		*Recipient
	}{}

	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, err
	}

	// if the "signature" field exist, treat it as a flattened
	if m.Recipient != nil {
		if len(m.Message.Recipients) != 0 {
			return nil, errors.New("invalid message: mixed flattened/full json serialization")
		}

		m.Message.Recipients = []Recipient{*m.Recipient}
	}

	return m.Message, nil
}

func parseCompact(buf []byte) (*Message, error) {
	debug.Printf("Parse(Compact): buf = '%s'", buf)
	parts := bytes.Split(buf, []byte{'.'})
	if len(parts) != 5 {
		return nil, ErrInvalidCompactPartsCount
	}

	enc := base64.RawURLEncoding
	p0Len := enc.DecodedLen(len(parts[0]))
	p1Len := enc.DecodedLen(len(parts[1]))
	p2Len := enc.DecodedLen(len(parts[2]))
	p3Len := enc.DecodedLen(len(parts[3]))
	p4Len := enc.DecodedLen(len(parts[4]))

	out := make([]byte, p0Len+p1Len+p2Len+p3Len+p4Len)

	hdrbuf := buffer.Buffer(out[:p0Len])
	if _, err := enc.Decode(hdrbuf, parts[0]); err != nil {
		return nil, err
	}
	hdrbuf = bytes.TrimRight(hdrbuf, "\x00")
	debug.Printf("p0     = %x", out[:p0Len])
	debug.Printf("hdrbuf = %x", hdrbuf)

	hdr := NewHeader()
	if err := json.Unmarshal(hdrbuf, hdr); err != nil {
		return nil, err
	}

	// We need the protected header to contain the content encryption
	// algorithm. XXX probably other headers need to go there too
	protected := NewEncodedHeader()
	protected.ContentEncryption = hdr.ContentEncryption
	hdr.ContentEncryption = ""

	enckeybuf := buffer.Buffer(out[p0Len : p0Len+p1Len])
	if _, err := enc.Decode(enckeybuf, parts[1]); err != nil {
		return nil, err
	}
	enckeybuf = bytes.TrimRight(enckeybuf, "\x00")

	ivbuf := buffer.Buffer(out[p0Len+p1Len : p0Len+p1Len+p2Len])
	if _, err := enc.Decode(ivbuf, parts[2]); err != nil {
		return nil, err
	}
	ivbuf = bytes.TrimRight(ivbuf, "\x00")

	ctbuf := buffer.Buffer(out[p0Len+p1Len+p2Len : p0Len+p1Len+p2Len+p3Len])
	if _, err := enc.Decode(ctbuf, parts[3]); err != nil {
		return nil, err
	}
	ctbuf = bytes.TrimRight(ctbuf, "\x00")

	tagbuf := buffer.Buffer(out[p0Len+p1Len+p2Len+p3Len : p0Len+p1Len+p2Len+p3Len+p4Len])
	if _, err := enc.Decode(tagbuf, parts[4]); err != nil {
		return nil, err
	}
	tagbuf = bytes.TrimRight(tagbuf, "\x00")

	m := NewMessage()
	/*
		v, err := protected.Base64Encode()
		if err != nil {
			return nil, err
		}
		m.AuthenticatedData.Base64Decode(v)
	*/
	m.AuthenticatedData.SetBytes(hdrbuf.Bytes())
	m.ProtectedHeader = protected
	m.Tag = tagbuf
	m.CipherText = ctbuf
	m.InitializationVector = ivbuf
	m.Recipients = []Recipient{
		Recipient{
			Header:       hdr,
			EncryptedKey: enckeybuf,
		},
	}
	return m, nil
}

// BuildKeyDecrypter creates a new KeyDecrypter instance from the given
// parameters. It is used by the DecryptMessage fucntion to create
// key decrypter(s) from the given message. `keysize` is only used by
// some decrypters. Use the value from ContentCipher.KeySize().
func BuildKeyDecrypter(alg jwa.KeyEncryptionAlgorithm, key interface{}, keysize int) (KeyDecrypter, error) {
	switch alg {
	case jwa.RSA1_5:
		privkey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("*rsa.PrivateKey is required as the key to build this key decrypter")
		}
		return NewRSAPKCS15KeyDecrypt(alg, privkey, keysize), nil
	case jwa.RSA_OAEP, jwa.RSA_OAEP_256:
		privkey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("*rsa.PrivateKey is required as the key to build this key decrypter")
		}
		return NewRSAOAEPKeyDecrypt(alg, privkey), nil
	case jwa.A128KW, jwa.A192KW, jwa.A256KW:
		sharedkey, ok := key.([]byte)
		if !ok {
			return nil, errors.New("[]byte is required as the key to build this key decrypter")
		}
		return NewAesKeyWrap(alg, sharedkey)
	}

	return nil, NewErrUnsupportedAlgorithm(string(alg), "key decryption")
}

func BuildContentCipher(alg jwa.ContentEncryptionAlgorithm) (ContentCipher, error) {
	switch alg {
	case jwa.A128GCM, jwa.A192GCM, jwa.A256GCM, jwa.A128CBC_HS256, jwa.A192CBC_HS384, jwa.A256CBC_HS512:
		return NewAesContentCipher(alg)
	}

	return nil, ErrUnsupportedAlgorithm
}

func DecryptMessage(m *Message, key interface{}) ([]byte, error) {
	var err error

	if len(m.Recipients) == 0 {
		return nil, errors.New("no recipients, can not proceed with decrypt")
	}

	enc := m.ProtectedHeader.ContentEncryption

	h := NewHeader()
	if err := h.Copy(m.ProtectedHeader.Header); err != nil {
		return nil, err
	}
	h, err = h.Merge(m.UnprotectedHeader)
	if err != nil {
		debug.Printf("failed to merge unprotected header")
		return nil, err
	}

	debug.Printf("DecryptMessage: aad (bytes)   = %s", m.AuthenticatedData.Bytes())
	{
		b64, _ := m.AuthenticatedData.Base64Encode()
		debug.Printf("DecryptMessage: aad (encoded) = %s", b64)
	}

	// Now, this is weird. If Message contains 1

	aad, err := m.AuthenticatedData.Base64Encode()
	if err != nil {
		return nil, err
	}
	ciphertext := m.CipherText.Bytes()
	iv := m.InitializationVector.Bytes()
	tag := m.Tag.Bytes()

	cipher, err := BuildContentCipher(enc)
	if err != nil {
		return nil, fmt.Errorf("unsupported content cipher algorithm '%s'", enc)
	}
	keysize := cipher.KeySize()
	debug.Printf("cipher.keysize = %d", keysize)

	var plaintext []byte
	for _, recipient := range m.Recipients {
		h2 := NewHeader()
		if err := h2.Copy(h); err != nil {
			debug.Printf("failed to copy header: %s", err)
			continue
		}

		h2, err := h2.Merge(recipient.Header)
		if err != nil {
			debug.Printf("Failed to merge! %s", err)
			continue
		}

		k, err := BuildKeyDecrypter(h2.Algorithm, key, keysize)
		if err != nil {
			debug.Printf("failed to create key decrypter: %s", err)
			continue
		}

		debug.Printf("DecryptMessage: encrypted_key = %x", recipient.EncryptedKey.Bytes())
		cek, err := k.KeyDecrypt(recipient.EncryptedKey.Bytes())
		if err != nil {
			debug.Printf("failed to decrypt key: %s", err)
			continue
		}

		debug.Printf("DecryptMessage: cek        = %x (%d)", cek, len(cek))
		debug.Printf("DecryptMessage: iv         = %x", iv)
		debug.Printf("DecryptMessage: ciphertext = %x", ciphertext)
		debug.Printf("DecryptMessage: tag        = %x", tag)
		debug.Printf("DecryptMessage: aad        = %x", aad)
		plaintext, err = cipher.decrypt(cek, iv, ciphertext, tag, aad)
		if err == nil {
			break
		}
		debug.Printf("DecryptMessage: cipher.decrypt: %s", err)
	}

	if plaintext == nil {
		return nil, errors.New("failed to decrypt key")
	}

	if h.Compression == jwa.Deflate {
		output := bytes.Buffer{}
		w, _ := flate.NewWriter(&output, 1)
		in := plaintext
		for len(in) > 0 {
			n, err := w.Write(in)
			if err != nil {
				return nil, err
			}
			in = in[n:]
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		plaintext = output.Bytes()
	}

	return plaintext, nil
}

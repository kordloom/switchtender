package credential

import (
	"crypto/hmac"
	"crypto/sha256"
)

// MAC returns the HMAC-SHA256 of data under a key derived from the sealer's key for purpose, so a
// caller can sign a value only this server can mint, and check one, without ever holding the key.
// Each purpose derives its own key, so a MAC made for one purpose never verifies for another. It
// returns ErrNoKey when the sealer has no key.
func (s *Sealer) MAC(purpose string, data []byte) ([]byte, error) {
	if s == nil || !s.ok {
		return nil, ErrNoKey
	}
	derive := hmac.New(sha256.New, s.key[:])
	derive.Write([]byte("switchtender-mac\x00" + purpose))
	key := derive.Sum(nil)
	defer clear(key)
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil), nil
}

package store

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// DeviceKey: the software ECDSA P-256 key that signs /jobs/* requests
// (desktop counterpart of the Android Keystore key). Created on first use;
// the private half never leaves the data directory.
func (s *Store) DeviceKey() (*ecdsa.PrivateKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.path(fileKey)
	if raw, err := os.ReadFile(p); err == nil {
		plain, err := unprotect(raw)
		if err != nil {
			return nil, fmt.Errorf("ключ устройства не читается: %w", err)
		}
		k, err := x509.ParsePKCS8PrivateKey(plain)
		if err != nil {
			return nil, fmt.Errorf("ключ устройства повреждён: %w", err)
		}
		ek, ok := k.(*ecdsa.PrivateKey)
		if !ok || ek.Curve != elliptic.P256() {
			return nil, errors.New("ключ устройства: не ECDSA P-256")
		}
		return ek, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	sealed, err := protect(der)
	if err != nil {
		return nil, err
	}
	if err := writeFile(p, sealed); err != nil {
		return nil, err
	}
	return k, nil
}

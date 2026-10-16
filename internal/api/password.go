package api

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

// Admin passwords are stored as pbkdf2-sha256:<iterations>:<salt>:<key>, with the
// salt and key in unpadded base64url. `kube-sre admin-hash` makes them.

const (
	hashScheme     = "pbkdf2-sha256"
	hashIterations = 600000
	hashSaltLen    = 16
	hashKeyLen     = 32
	// MinPasswordLen is the shortest password admin-hash accepts.
	MinPasswordLen = 12
)

// HashPassword returns a storable hash of password with a fresh salt.
func HashPassword(password string) (string, error) {
	if len([]rune(password)) < MinPasswordLen {
		return "", errors.New("use at least 12 characters")
	}
	salt := make([]byte, hashSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return hashPassword(password, salt, hashIterations)
}

func hashPassword(password string, salt []byte, iterations int) (string, error) {
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, hashKeyLen)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return hashScheme + ":" + strconv.Itoa(iterations) + ":" + enc.EncodeToString(salt) + ":" + enc.EncodeToString(key), nil
}

func parseHash(h string) (iterations int, salt, key []byte, ok bool) {
	parts := strings.Split(strings.TrimSpace(h), ":")
	if len(parts) != 4 || parts[0] != hashScheme {
		return 0, nil, nil, false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil || n < 1 {
		return 0, nil, nil, false
	}
	enc := base64.RawURLEncoding
	salt, err1 := enc.DecodeString(parts[2])
	key, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(salt) == 0 || len(key) == 0 {
		return 0, nil, nil, false
	}
	return n, salt, key, true
}

// ValidHash reports whether h is a hash made by HashPassword.
func ValidHash(h string) bool {
	_, _, _, ok := parseHash(h)
	return ok
}

// verifyPassword checks password against h in constant time.
func verifyPassword(h, password string) bool {
	n, salt, want, ok := parseHash(h)
	if !ok {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, n, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

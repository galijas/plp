// Package auth holds password hashing and random token generation.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const (
	bcryptCost     = 12
	MinPasswordLen = 12
	// InviteCodeLen is the length of generated invite codes. Codes are
	// grouped in fours for reading aloud or copying from an email.
	InviteCodeLen = 16
)

const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// Invite codes avoid characters that are easy to confuse (0/O, 1/I/l).
const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randomFrom(alphabet string, n int) string {
	var b strings.Builder
	max := big.NewInt(int64(len(alphabet)))
	for i := 0; i < n; i++ {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		b.WriteByte(alphabet[v.Int64()])
	}
	return b.String()
}

// RandomString returns n characters from [A-Za-z0-9].
func RandomString(n int) string { return randomFrom(alnum, n) }

// NewInviteCode returns a code like "K7QM-2XRT-9PWD-HN4C" (80 bits).
func NewInviteCode() string {
	raw := randomFrom(codeAlphabet, InviteCodeLen)
	var parts []string
	for i := 0; i < len(raw); i += 4 {
		parts = append(parts, raw[i:i+4])
	}
	return strings.Join(parts, "-")
}

// NormalizeCode makes code comparison forgiving about case and surrounding
// spaces, which is how people paste codes from emails.
func NormalizeCode(c string) string { return strings.ToUpper(strings.TrimSpace(c)) }

// HashToken hashes high-entropy secrets (session tokens). A fast hash is
// enough: these are random, not chosen by people.
func HashToken(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func NewSessionToken() string { return RandomString(43) }

func NewID() string { return RandomString(24) }

func GeneratePassword() string { return RandomString(20) }

func HashPassword(pw string) (string, error) {
	if len(pw) < MinPasswordLen {
		return "", errors.New("the password must be at least 12 characters")
	}
	if len(pw) > 72 {
		return "", errors.New("the password must be at most 72 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	return string(h), err
}

// dummyHash is compared against when the username doesn't exist, so a
// failed login takes the same time either way.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("plportal-dummy-password"), bcryptCost)

func CheckPassword(hash, pw string) bool {
	if hash == "" {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(pw))
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

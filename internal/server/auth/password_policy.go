package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MinPasswordRunes = 15
	MaxPasswordRunes = 128
	MaxPasswordBytes = 512
)

// ErrPasswordPolicy identifies a rejected new password. Verification of an
// existing password deliberately does not apply the creation policy.
var ErrPasswordPolicy = errors.New("password does not meet the security policy")

// ValidatePassword is the shared policy for creating or replacing a management
// password. Passphrases and Unicode are supported; character classes are not
// required. It does not trim or otherwise change the password that is hashed.
func ValidatePassword(password string) error {
	if !utf8.ValidString(password) {
		return fmt.Errorf("%w: use valid UTF-8 text", ErrPasswordPolicy)
	}
	count := utf8.RuneCountInString(password)
	if count < MinPasswordRunes || count > MaxPasswordRunes || len(password) > MaxPasswordBytes {
		return fmt.Errorf("%w: use %d to %d characters, at most %d bytes", ErrPasswordPolicy, MinPasswordRunes, MaxPasswordRunes, MaxPasswordBytes)
	}
	for _, r := range password {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: control characters are not allowed", ErrPasswordPolicy)
		}
	}
	normalized := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, password)
	base := strings.TrimFunc(normalized, unicode.IsNumber)
	pattern := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, password)
	if pattern == "" || commonPassword(base) || repeatedPassword([]rune(pattern)) || sequentialPassword(pattern) {
		return fmt.Errorf("%w: choose an uncommon password or passphrase instead of a common word, repetition or sequence", ErrPasswordPolicy)
	}
	return nil
}

func commonPassword(password string) bool {
	switch password {
	case "password", "passwordpassword", "passwordpasswordpassword", "admin", "adminpassword", "administrator",
		"qwerty", "qwertyuiop", "letmein", "welcome", "welcomewelcome", "changeme", "changeyourpassword",
		"defaultpassword", "thisisapassword", "iloveyou", "iloveyouforever", "gpuworks", "gpuworkspassword":
		return true
	}
	return false
}

// repeatedPassword rejects an entire password made of the same short unit,
// while leaving repetitions within an otherwise meaningful passphrase alone.
func repeatedPassword(password []rune) bool {
	if len(password) < 2 {
		return true
	}
	for size := 1; size <= 8 && size <= len(password)/2; size++ {
		if len(password)%size != 0 {
			continue
		}
		repeated := true
		for i := size; i < len(password); i++ {
			if password[i] != password[i%size] {
				repeated = false
				break
			}
		}
		if repeated {
			return true
		}
	}
	return false
}

func sequentialPassword(password string) bool {
	for _, sequence := range []string{"0123456789", "9876543210", "abcdefghijklmnopqrstuvwxyz", "zyxwvutsrqponmlkjihgfedcba", "1234567890qwertyuiopasdfghjklzxcvbnm", "mnbvcxzlkjhgfdsapoiuytrewq0987654321", "qwertyuiop", "poiuytrewq", "asdfghjkl", "lkjhgfdsa", "zxcvbnm", "mnbvcxz"} {
		if strings.Contains(strings.Repeat(sequence, len(password)/len(sequence)+2), password) {
			return true
		}
	}
	return false
}

// NewPassword supplies a fresh management password with 192 bits of entropy.
// Entropy failure is returned to the caller before an account or file is saved.
func NewPassword() (string, error) {
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate admin password: %w", err)
	}
	password := hex.EncodeToString(random[:])
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	return password, nil
}

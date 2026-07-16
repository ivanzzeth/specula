package auth

import "golang.org/x/crypto/bcrypt"

// MinPasswordLen is the minimum plaintext password length enforced at
// registration and password-change time. Passwords shorter than this are
// rejected before reaching the bcrypt layer.
const MinPasswordLen = 8

// dummyPlain / dummyHash provide a pre-computed pair used for timing
// equalization. When a login attempt uses a non-existent email the "user not
// found" branch would otherwise return almost instantly, leaking whether the
// email is registered via a response-latency side-channel. CheckPasswordDummy
// runs one full bcrypt comparison of equal cost so the branch is
// indistinguishable from a real lookup+compare (OWASP Auth Cheat Sheet §1.9).
var (
	dummyPlain   = []byte("constant-time-dummy-password-specula")
	dummyHash, _ = bcrypt.GenerateFromPassword(dummyPlain, bcrypt.DefaultCost)
)

// CheckPasswordDummy runs a bcrypt comparison against the pre-computed dummy
// hash and discards the result. Call this in the "user not found" branch of
// Login to absorb the bcrypt-shaped CPU timing gap and prevent user enumeration
// via timing side-channel attacks.
func CheckPasswordDummy() {
	_ = bcrypt.CompareHashAndPassword(dummyHash, dummyPlain)
}

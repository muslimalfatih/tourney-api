package auth

import "strings"

// NormalizeEmail is the single definition of "the same address" across this
// service: the invitation allowlist, the users table, OTP challenges and rate
// limit buckets must all agree, or an invited person is refused because they
// capitalised their own address.
//
// Trim and lowercase only. Deliberately NOT applied:
//
//   - stripping dots or +tags. Those are Gmail conventions, not standards.
//     Treating a+b@x.com as a@x.com would let one invitation admit addresses
//     the admin never approved, which is the opposite of an allowlist.
//   - Unicode normalization. Nothing here compares homoglyphs, and quietly
//     rewriting someone's address is worse than refusing it.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

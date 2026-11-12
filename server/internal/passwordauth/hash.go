package passwordauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Algo is the only algorithm user_credential.algo accepts.
const Algo = "argon2id"

const (
	// MinPasswordRunes is the minimum password length, counted in characters.
	MinPasswordRunes = 12
	// MaxPasswordBytes caps the input to argon2id so a huge request body
	// cannot be used to burn CPU. No real password comes near it.
	MaxPasswordBytes = 1024
)

// Errors returned by this package. They are fixed values and never carry the
// password or the hash.
var (
	ErrPasswordTooShort = fmt.Errorf("passwordauth: password must be at least %d characters", MinPasswordRunes)
	ErrPasswordTooLong  = fmt.Errorf("passwordauth: password must be at most %d bytes", MaxPasswordBytes)
	ErrMalformedHash    = errors.New("passwordauth: stored hash is malformed")
	ErrUnsupportedHash  = errors.New("passwordauth: stored hash uses an unsupported algorithm or version")
)

// ValidatePassword checks a new password against the length rules. Login does
// not call it: a stored password is checked as is.
func ValidatePassword(password string) error {
	if len(password) > MaxPasswordBytes {
		return ErrPasswordTooLong
	}
	if utf8.RuneCountInString(password) < MinPasswordRunes {
		return ErrPasswordTooShort
	}
	return nil
}

// Params are the argon2id cost parameters. The JSON form is what
// user_credential.params stores.
type Params struct {
	MemoryKiB   uint32 `json:"m"`
	Iterations  uint32 `json:"t"`
	Parallelism uint8  `json:"p"`
	SaltLen     uint32 `json:"salt_len"`
	KeyLen      uint32 `json:"key_len"`
}

// DefaultParams is RFC 9106 section 4's second recommended option (t=3,
// 64 MiB, 4 lanes), meant for servers that cannot spend 2 GiB per hash; it is
// above the OWASP Password Storage Cheat Sheet minimum (19 MiB, t=2, p=1).
// Salt and key lengths are RFC 9106's 128-bit salt and 256-bit tag.
var DefaultParams = Params{
	MemoryKiB:   64 * 1024,
	Iterations:  3,
	Parallelism: 4,
	SaltLen:     16,
	KeyLen:      32,
}

// Environment variables that override DefaultParams.
const (
	EnvMemoryKiB   = "MULTICA_PASSWORD_ARGON2_MEMORY_KIB"
	EnvIterations  = "MULTICA_PASSWORD_ARGON2_ITERATIONS"
	EnvParallelism = "MULTICA_PASSWORD_ARGON2_PARALLELISM"
)

// Bounds for operator-chosen parameters. The floors keep an override at or
// above the OWASP minimum cost (19 MiB, and memory times passes at least that
// of 19 MiB with t=2). The ceilings catch typos that would exhaust memory or
// stall logins; a stored hash outside them is treated as malformed.
const (
	minMemoryKiB     = 19 * 1024
	minMemoryPasses  = 2 * minMemoryKiB
	maxMemoryKiB     = 1024 * 1024
	maxIterations    = 64
	minSaltLen       = 16
	maxSaltLen       = 64
	minKeyLen        = 16
	maxKeyLen        = 64
	minTestMemoryKiB = 8 // argon2 needs at least 8 KiB per lane
)

// ParamsFromEnv returns DefaultParams with the environment overrides applied.
// getenv is os.Getenv in production. An override below the OWASP minimum or
// above the ceilings is an error, so a misconfigured server fails at startup
// instead of storing weak hashes.
func ParamsFromEnv(getenv func(string) string) (Params, error) {
	p := DefaultParams
	if v := strings.TrimSpace(getenv(EnvMemoryKiB)); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil || n < minMemoryKiB || n > maxMemoryKiB {
			return Params{}, fmt.Errorf("passwordauth: %s must be an integer from %d to %d", EnvMemoryKiB, minMemoryKiB, maxMemoryKiB)
		}
		p.MemoryKiB = uint32(n)
	}
	if v := strings.TrimSpace(getenv(EnvIterations)); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil || n < 1 || n > maxIterations {
			return Params{}, fmt.Errorf("passwordauth: %s must be an integer from 1 to %d", EnvIterations, maxIterations)
		}
		p.Iterations = uint32(n)
	}
	if v := strings.TrimSpace(getenv(EnvParallelism)); v != "" {
		n, err := strconv.ParseUint(v, 10, 8)
		if err != nil || n < 1 {
			return Params{}, fmt.Errorf("passwordauth: %s must be an integer from 1 to 255", EnvParallelism)
		}
		p.Parallelism = uint8(n)
	}
	if uint64(p.MemoryKiB)*uint64(p.Iterations) < minMemoryPasses {
		return Params{}, fmt.Errorf("passwordauth: %s times %s must be at least %d", EnvMemoryKiB, EnvIterations, minMemoryPasses)
	}
	if err := p.validate(); err != nil {
		return Params{}, err
	}
	return p, nil
}

// validate checks what argon2id itself and the decoder require. It does not
// apply the OWASP floor, so tests can use cheap parameters.
func (p Params) validate() error {
	switch {
	case p.Iterations < 1 || p.Iterations > maxIterations,
		p.Parallelism < 1,
		p.MemoryKiB < minTestMemoryKiB*uint32(p.Parallelism) || p.MemoryKiB > maxMemoryKiB,
		p.SaltLen < minSaltLen || p.SaltLen > maxSaltLen,
		p.KeyLen < minKeyLen || p.KeyLen > maxKeyLen:
		return fmt.Errorf("passwordauth: invalid argon2id parameters m=%d t=%d p=%d salt=%d key=%d",
			p.MemoryKiB, p.Iterations, p.Parallelism, p.SaltLen, p.KeyLen)
	}
	return nil
}

// Hasher hashes and verifies passwords with one set of current parameters.
type Hasher struct {
	params Params
	// dummy is a hash of a random password, verified when the account has no
	// credential so that case costs the same time as a wrong password.
	dummy string
}

// NewHasher returns a Hasher producing hashes with p.
func NewHasher(p Params) (*Hasher, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	h := &Hasher{params: p}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("passwordauth: read random: %w", err)
	}
	dummy, err := h.hash(base64.RawStdEncoding.EncodeToString(random))
	if err != nil {
		return nil, err
	}
	h.dummy = dummy
	return h, nil
}

// Params returns the parameters new hashes are made with.
func (h *Hasher) Params() Params { return h.params }

// Hash validates password and returns its PHC-format argon2id hash:
// $argon2id$v=19$m=<KiB>,t=<passes>,p=<lanes>$<salt>$<key>, with the salt and
// key in unpadded standard base64.
func (h *Hasher) Hash(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	return h.hash(password)
}

func (h *Hasher) hash(password string) (string, error) {
	salt := make([]byte, h.params.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("passwordauth: read random: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, h.params.Iterations, h.params.MemoryKiB, h.params.Parallelism, h.params.KeyLen)
	return encode(h.params, salt, key), nil
}

// Verify reports whether password matches encoded. The key comparison takes
// constant time. A password over MaxPasswordBytes never matches and is not
// hashed. An error means encoded itself is unusable.
func (h *Hasher) Verify(password, encoded string) (bool, error) {
	p, salt, key, err := decode(encoded)
	if err != nil {
		return false, err
	}
	if len(password) > MaxPasswordBytes {
		return false, nil
	}
	got := argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKiB, p.Parallelism, p.KeyLen)
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

// VerifyDummy spends the time of one verification and always fails. Callers
// use it when the account has no credential, so response time does not tell
// whether a password is set.
func (h *Hasher) VerifyDummy(password string) {
	_, _ = h.Verify(password, h.dummy)
}

// NeedsRehash reports whether encoded was made with parameters other than the
// current ones. After a successful login the caller re-hashes the password it
// just verified, so a parameter change reaches every active account.
func (h *Hasher) NeedsRehash(encoded string) bool {
	p, _, _, err := decode(encoded)
	return err != nil || p != h.params
}

func encode(p Params, salt, key []byte) string {
	return fmt.Sprintf("$%s$v=%d$m=%d,t=%d,p=%d$%s$%s",
		Algo, argon2.Version, p.MemoryKiB, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

// decode parses a PHC string written by encode. Errors name no part of the
// input.
func decode(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" {
		return Params{}, nil, nil, ErrMalformedHash
	}
	if parts[1] != Algo || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return Params{}, nil, nil, ErrUnsupportedHash
	}
	var p Params
	fields := strings.Split(parts[3], ",")
	if len(fields) != 3 {
		return Params{}, nil, nil, ErrMalformedHash
	}
	for i, name := range []string{"m", "t", "p"} {
		value, ok := strings.CutPrefix(fields[i], name+"=")
		if !ok {
			return Params{}, nil, nil, ErrMalformedHash
		}
		bits := 32
		if name == "p" {
			bits = 8
		}
		n, err := strconv.ParseUint(value, 10, bits)
		if err != nil {
			return Params{}, nil, nil, ErrMalformedHash
		}
		switch name {
		case "m":
			p.MemoryKiB = uint32(n)
		case "t":
			p.Iterations = uint32(n)
		case "p":
			p.Parallelism = uint8(n)
		}
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, ErrMalformedHash
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, ErrMalformedHash
	}
	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(key))
	if p.validate() != nil {
		return Params{}, nil, nil, ErrMalformedHash
	}
	return p, salt, key, nil
}

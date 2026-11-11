package passwordauth

import (
	"errors"
	"strings"
	"testing"
)

// cheapParams keeps unit tests fast. Production parameters come from
// ParamsFromEnv, which refuses anything this weak.
var cheapParams = Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}

const testPassword = "correct horse battery"

func newTestHasher(t *testing.T, p Params) *Hasher {
	t.Helper()
	h, err := NewHasher(p)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHashRoundTrip(t *testing.T) {
	h := newTestHasher(t, cheapParams)
	encoded, err := h.Hash(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("hash is not a PHC argon2id string with the current parameters")
	}
	if strings.Contains(encoded, testPassword) {
		t.Fatal("hash contains the password")
	}

	ok, err := h.Verify(testPassword, encoded)
	if err != nil || !ok {
		t.Fatalf("correct password: ok=%v err=%v", ok, err)
	}
	for _, wrong := range []string{"correct horse batterY", "correct horse battery ", "", testPassword[:12]} {
		if ok, err := h.Verify(wrong, encoded); err != nil || ok {
			t.Errorf("wrong password %q: ok=%v err=%v", wrong, ok, err)
		}
	}

	again, err := h.Hash(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if again == encoded {
		t.Fatal("two hashes of one password are equal: salt is not random")
	}
	if ok, _ := h.Verify(testPassword, again); !ok {
		t.Fatal("second hash does not verify")
	}
}

func TestValidatePassword(t *testing.T) {
	cases := []struct {
		name     string
		password string
		want     error
	}{
		{"empty", "", ErrPasswordTooShort},
		{"11 characters", "abcdefghijk", ErrPasswordTooShort},
		{"12 characters", "abcdefghijkl", nil},
		// Length counts characters, not bytes: 11 CJK characters are 33
		// bytes and still too short.
		{"11 multibyte characters", strings.Repeat("密", 11), ErrPasswordTooShort},
		{"12 multibyte characters", strings.Repeat("密", 12), nil},
		{"at the byte limit", strings.Repeat("a", MaxPasswordBytes), nil},
		{"over the byte limit", strings.Repeat("a", MaxPasswordBytes+1), ErrPasswordTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidatePassword(tc.password); !errors.Is(err, tc.want) {
				t.Fatalf("ValidatePassword = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestHashRejectsInvalidPasswords(t *testing.T) {
	h := newTestHasher(t, cheapParams)
	short := "short pw 11"
	encoded, err := h.Hash(short)
	if !errors.Is(err, ErrPasswordTooShort) || encoded != "" {
		t.Fatalf("short password: hash=%q err=%v", encoded, err)
	}
	if strings.Contains(err.Error(), short) {
		t.Fatal("error contains the password")
	}
	if _, err := h.Hash(strings.Repeat("x", MaxPasswordBytes+1)); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("long password: err=%v", err)
	}
}

func TestVerifyOverlongPasswordNeverMatches(t *testing.T) {
	h := newTestHasher(t, cheapParams)
	long := strings.Repeat("a", MaxPasswordBytes)
	encoded, err := h.Hash(long)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := h.Verify(long+"a", encoded); ok || err != nil {
		t.Fatalf("overlong password: ok=%v err=%v", ok, err)
	}
}

func TestNeedsRehashAfterParameterUpgrade(t *testing.T) {
	old := newTestHasher(t, cheapParams)
	encoded, err := old.Hash(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if old.NeedsRehash(encoded) {
		t.Fatal("hash made with the current parameters needs rehash")
	}

	upgraded := cheapParams
	upgraded.MemoryKiB, upgraded.Iterations = 128, 2
	current := newTestHasher(t, upgraded)
	if !current.NeedsRehash(encoded) {
		t.Fatal("hash made with older parameters does not need rehash")
	}
	// The old hash still verifies: parameters are read from the hash.
	if ok, err := current.Verify(testPassword, encoded); !ok || err != nil {
		t.Fatalf("old hash under new parameters: ok=%v err=%v", ok, err)
	}
	fresh, err := current.Hash(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if current.NeedsRehash(fresh) {
		t.Fatal("fresh hash needs rehash")
	}
	if !strings.Contains(fresh, "$m=128,t=2,p=1$") {
		t.Fatal("fresh hash does not carry the upgraded parameters")
	}

	// Any difference counts, including salt and key length.
	longerKey := cheapParams
	longerKey.KeyLen = 64
	if !newTestHasher(t, longerKey).NeedsRehash(encoded) {
		t.Fatal("key length change does not need rehash")
	}
	if !old.NeedsRehash("not a hash") {
		t.Fatal("malformed hash does not need rehash")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	h := newTestHasher(t, cheapParams)
	valid, err := h.Hash(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(valid, "$")
	salt, key := parts[4], parts[5]
	cases := []struct {
		name    string
		encoded string
		want    error
	}{
		{"empty", "", ErrMalformedHash},
		{"plain text", testPassword, ErrMalformedHash},
		{"bcrypt", "$2a$10$abcdefghijklmnopqrstuuABCDEFGHIJKLMNOPQRSTUVWXYZ01234", ErrMalformedHash},
		{"argon2i", "$argon2i$v=19$m=64,t=1,p=1$" + salt + "$" + key, ErrUnsupportedHash},
		{"old version", "$argon2id$v=16$m=64,t=1,p=1$" + salt + "$" + key, ErrUnsupportedHash},
		{"missing field", "$argon2id$v=19$m=64,t=1$" + salt + "$" + key, ErrMalformedHash},
		{"reordered fields", "$argon2id$v=19$t=1,m=64,p=1$" + salt + "$" + key, ErrMalformedHash},
		{"bad number", "$argon2id$v=19$m=x,t=1,p=1$" + salt + "$" + key, ErrMalformedHash},
		{"zero passes", "$argon2id$v=19$m=64,t=0,p=1$" + salt + "$" + key, ErrMalformedHash},
		{"memory too large", "$argon2id$v=19$m=4294967295,t=1,p=1$" + salt + "$" + key, ErrMalformedHash},
		{"bad salt", "$argon2id$v=19$m=64,t=1,p=1$!!!$" + key, ErrMalformedHash},
		{"short salt", "$argon2id$v=19$m=64,t=1,p=1$AAAA$" + key, ErrMalformedHash},
		{"padded key", "$argon2id$v=19$m=64,t=1,p=1$" + salt + "$" + key + "=", ErrMalformedHash},
		{"extra part", valid + "$x", ErrMalformedHash},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := h.Verify(testPassword, tc.encoded)
			if ok || !errors.Is(err, tc.want) {
				t.Fatalf("Verify = %v, %v; want false, %v", ok, err, tc.want)
			}
			if strings.Contains(err.Error(), testPassword) || (len(tc.encoded) > 0 && strings.Contains(err.Error(), tc.encoded)) {
				t.Fatal("error contains the password or the hash")
			}
		})
	}
}

func TestNewHasherRejectsInvalidParams(t *testing.T) {
	for name, mutate := range map[string]func(*Params){
		"zero passes":      func(p *Params) { p.Iterations = 0 },
		"zero lanes":       func(p *Params) { p.Parallelism = 0 },
		"memory per lane":  func(p *Params) { p.Parallelism = 9 },
		"short salt":       func(p *Params) { p.SaltLen = 8 },
		"short key":        func(p *Params) { p.KeyLen = 8 },
		"memory too large": func(p *Params) { p.MemoryKiB = maxMemoryKiB + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			p := cheapParams
			mutate(&p)
			if _, err := NewHasher(p); err == nil {
				t.Fatal("NewHasher accepted invalid parameters")
			}
		})
	}
}

func TestVerifyDummyNeverMatches(t *testing.T) {
	h := newTestHasher(t, cheapParams)
	h.VerifyDummy(testPassword)
	if ok, err := h.Verify(testPassword, h.dummy); ok || err != nil {
		t.Fatalf("dummy hash matched: ok=%v err=%v", ok, err)
	}
}

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParamsFromEnv(t *testing.T) {
	p, err := ParamsFromEnv(envMap(nil))
	if err != nil || p != DefaultParams {
		t.Fatalf("no overrides: %+v %v", p, err)
	}
	if _, err := NewHasher(DefaultParams); err != nil {
		t.Fatalf("DefaultParams invalid: %v", err)
	}

	p, err = ParamsFromEnv(envMap(map[string]string{
		EnvMemoryKiB:   " 47104 ",
		EnvIterations:  "1",
		EnvParallelism: "2",
	}))
	want := Params{MemoryKiB: 47104, Iterations: 1, Parallelism: 2, SaltLen: 16, KeyLen: 32}
	if err != nil || p != want {
		t.Fatalf("overrides: %+v %v; want %+v", p, err, want)
	}

	for name, env := range map[string]map[string]string{
		"memory below OWASP minimum":  {EnvMemoryKiB: "19455"},
		"memory times passes too low": {EnvMemoryKiB: "19456", EnvIterations: "1"},
		"memory above ceiling":        {EnvMemoryKiB: "1048577"},
		"memory not a number":         {EnvMemoryKiB: "64MiB"},
		"negative passes":             {EnvIterations: "-1"},
		"zero passes":                 {EnvIterations: "0"},
		"too many passes":             {EnvIterations: "65"},
		"zero lanes":                  {EnvParallelism: "0"},
		"lanes over 255":              {EnvParallelism: "256"},
		"lanes not a number":          {EnvParallelism: "four"},
	} {
		t.Run(name, func(t *testing.T) {
			if p, err := ParamsFromEnv(envMap(env)); err == nil {
				t.Fatalf("accepted %v as %+v", env, p)
			}
		})
	}
}

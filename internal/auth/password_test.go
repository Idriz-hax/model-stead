package auth

import "testing"

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("a sufficiently long password")
	if err != nil {
		t.Fatal(err)
	}
	if hash[:len("$argon2id$v=19$")] != "$argon2id$v=19$" {
		t.Fatalf("unexpected PHC encoding: %q", hash)
	}
	if ok, err := VerifyPassword(hash, "a sufficiently long password"); err != nil || !ok {
		t.Fatalf("verify correct password = %v, %v", ok, err)
	}
	if ok, err := VerifyPassword(hash, "wrong password"); err != nil || ok {
		t.Fatalf("verify wrong password = %v, %v", ok, err)
	}
}

func TestVerifyPasswordRejectsMalformedAndUnboundedPHC(t *testing.T) {
	for _, hash := range []string{"", "$argon2id$v=19$m=999999999,t=1,p=1$c2FsdA$aGFzaA", "$argon2i$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA"} {
		if _, err := VerifyPassword(hash, "password"); err == nil {
			t.Errorf("VerifyPassword(%q) expected error", hash)
		}
	}
}

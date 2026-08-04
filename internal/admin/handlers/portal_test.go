package handlers

import (
	"testing"
)

func TestCheckPassword(t *testing.T) {
	// Seed hash from migration 052 for password "admin123"
	seedHash := "$2a$10$CWeP43NFxYSMK0nF.qt8EuQL.a2MhGYjekLxAeQ3XtfJsJewxWUXe"

	if !checkPassword("admin123", seedHash) {
		t.Errorf("expected checkPassword to succeed for admin123 and seedHash, got false")
	}

	if checkPassword("wrongpassword", seedHash) {
		t.Errorf("expected checkPassword to fail for wrong password, got true")
	}

	// Dynamic generated hash
	hashed := hashPassword("mySecretPass123!")
	if !checkPassword("mySecretPass123!", hashed) {
		t.Errorf("expected checkPassword to succeed for dynamically hashed password")
	}
	if checkPassword("wrong", hashed) {
		t.Errorf("expected checkPassword to fail for wrong password")
	}
}

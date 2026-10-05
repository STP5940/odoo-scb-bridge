package database_test

import (
	"testing"
	"time"

	"odoo-scb-bridge/internal/database"
)

func TestSFTPSecurityLockoutDuration(t *testing.T) {
	db, err := database.Init(":memory:")
	if err != nil {
		t.Fatalf("database.Init: %v", err)
	}

	// 1. Check default settings: lockout_minutes should be 60, max_failed_attempts should be 5
	settings, _, err := db.GetSFTPAccessControl()
	if err != nil {
		t.Fatalf("GetSFTPAccessControl: %v", err)
	}
	if settings.LockoutMinutes != 60 {
		t.Errorf("expected default lockout 60 min, got %d", settings.LockoutMinutes)
	}
	if settings.MaxFailedAttempts != 5 {
		t.Errorf("expected default max failed attempts 5, got %d", settings.MaxFailedAttempts)
	}

	// 2. Change lockout duration to 120 minutes and keep 5 attempts
	if err := db.SetSFTPSecuritySettings("allow_all", 120, 5); err != nil {
		t.Fatalf("SetSFTPSecuritySettings: %v", err)
	}
	settings, _, err = db.GetSFTPAccessControl()
	if err != nil {
		t.Fatalf("GetSFTPAccessControl: %v", err)
	}
	if settings.LockoutMinutes != 120 {
		t.Errorf("expected lockout 120 min, got %d", settings.LockoutMinutes)
	}
	if settings.MaxFailedAttempts != 5 {
		t.Errorf("expected max failed attempts 5, got %d", settings.MaxFailedAttempts)
	}

	// 3. Test failure attempts
	now := time.Now()
	for i := 1; i <= 4; i++ {
		blocked, newlyBlocked, remaining, err := db.RecordSFTPAuthFailure("1.2.3.4", "test", now)
		if err != nil {
			t.Fatalf("RecordSFTPAuthFailure %d: %v", i, err)
		}
		if blocked || newlyBlocked || remaining != 5-i {
			t.Errorf("attempt %d: got blocked=%v newlyBlocked=%v remaining=%d", i, blocked, newlyBlocked, remaining)
		}
	}

	// 5th attempt triggers lockout for 120 minutes (7200 seconds)
	blocked, newlyBlocked, remaining, err := db.RecordSFTPAuthFailure("1.2.3.4", "test", now)
	if err != nil {
		t.Fatalf("RecordSFTPAuthFailure 5: %v", err)
	}
	if !blocked || !newlyBlocked || remaining != 120*60 {
		t.Errorf("attempt 5: expected blocked=true newlyBlocked=true remaining=%d, got blocked=%v newlyBlocked=%v remaining=%d", 120*60, blocked, newlyBlocked, remaining)
	}

	// 4. Verify blocked until
	until, err := db.GetSFTPIPBlockedUntil("1.2.3.4")
	if err != nil {
		t.Fatalf("GetSFTPIPBlockedUntil: %v", err)
	}
	diff := until.Sub(now.Add(120 * time.Minute))
	if diff < -time.Second || diff > time.Second {
		t.Errorf("blockedUntil diff too large: %v", diff)
	}

	// 5. Change max_failed_attempts to 3
	if err := db.SetSFTPSecuritySettings("allow_all", 30, 3); err != nil {
		t.Fatalf("SetSFTPSecuritySettings: %v", err)
	}
	ip2 := "5.6.7.8"
	now2 := time.Now()
	for i := 1; i <= 2; i++ {
		blocked, newlyBlocked, remaining, err := db.RecordSFTPAuthFailure(ip2, "test", now2)
		if err != nil {
			t.Fatalf("RecordSFTPAuthFailure %d: %v", i, err)
		}
		if blocked || newlyBlocked || remaining != 3-i {
			t.Errorf("attempt %d: got blocked=%v newlyBlocked=%v remaining=%d", i, blocked, newlyBlocked, remaining)
		}
	}
	// 3rd attempt should trigger lockout
	blocked, newlyBlocked, remaining, err = db.RecordSFTPAuthFailure(ip2, "test", now2)
	if err != nil {
		t.Fatalf("RecordSFTPAuthFailure 3: %v", err)
	}
	if !blocked || !newlyBlocked || remaining != 30*60 {
		t.Errorf("attempt 3: expected blocked=true newlyBlocked=true remaining=%d, got %v %v %d", 30*60, blocked, newlyBlocked, remaining)
	}

	// 6. Test max_failed_attempts = 0 (disabled)
	if err := db.SetSFTPSecuritySettings("allow_all", 30, 0); err != nil {
		t.Fatalf("SetSFTPSecuritySettings 0: %v", err)
	}
	ip3 := "9.10.11.12"
	now3 := time.Now()
	for i := 1; i <= 10; i++ {
		blocked, newlyBlocked, remaining, err := db.RecordSFTPAuthFailure(ip3, "test", now3)
		if err != nil {
			t.Fatalf("RecordSFTPAuthFailure with 0 max attempts: %v", err)
		}
		if blocked || newlyBlocked || remaining != 0 {
			t.Errorf("attempt %d with max=0: got blocked=%v newlyBlocked=%v remaining=%d", i, blocked, newlyBlocked, remaining)
		}
	}
}

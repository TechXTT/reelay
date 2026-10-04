package store

import (
	"context"
	"testing"
)

func TestProfileCreateRejectsCaseDuplicateGroups(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if err := Migrate(ctx, s, testLogger()); err != nil {
		t.Fatal(err)
	}

	bad := testProfile()
	bad.PreferredGroups = map[string]int{"NTb": 50, "ntb": 10}
	if _, err := s.Profiles().Create(ctx, bad); err == nil {
		t.Fatal("Create accepted preferred groups that differ only by case")
	}

	if _, err := s.Profiles().Create(ctx, testProfile()); err != nil {
		t.Fatalf("Create valid profile: %v", err)
	}
}

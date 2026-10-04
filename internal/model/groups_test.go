package model

import (
	"reflect"
	"testing"
)

func TestCaseDuplicateGroups(t *testing.T) {
	got := CaseDuplicateGroups(map[string]int{"NTb": 1, "ntb": 2, "FLUX": 3, "Yts": 1, "YTS": 2, "yts": 3})
	want := [][]string{{"NTb", "ntb"}, {"YTS", "Yts", "yts"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CaseDuplicateGroups = %v, want %v", got, want)
	}
	if got := CaseDuplicateGroups(map[string]int{"NTb": 1, "FLUX": 2}); len(got) != 0 {
		t.Errorf("CaseDuplicateGroups(no collisions) = %v, want none", got)
	}
}

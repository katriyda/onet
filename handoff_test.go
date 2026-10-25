package main

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestHandoffStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handoff.json")
	want := &handoffState{
		Version: handoffStateVersion,
		Seq:     7,
		Sessions: []handoffSession{{
			ID:      "sess-7",
			Name:    "demo",
			Cols:    120,
			Rows:    40,
			PID:     1234,
			Started: 5678,
			Buffer:  []byte{0, 1, 2, 255},
			Handles: []uint64{9, 10, 11, 12, 13, 14, 15, 16},
		}, {
			ID:       "dead",
			Dead:     true,
			ExitCode: 137,
		}},
		ListenerFD: 42,
		Rebind:     true,
	}
	if err := writeHandoffState(path, want); err != nil {
		t.Fatalf("writeHandoffState: %v", err)
	}
	got, err := loadHandoffState(path)
	if err != nil {
		t.Fatalf("loadHandoffState: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got %#v\nwant %#v", got, want)
	}
}

func TestHandoffStateRejectsVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := writeHandoffState(path, &handoffState{Version: handoffStateVersion + 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadHandoffState(path); err == nil {
		t.Fatal("loadHandoffState 应拒绝未知版本")
	}
}

func TestHandoffStateAcceptsV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.json")
	if err := writeHandoffState(path, &handoffState{Version: 1, Sessions: []handoffSession{{ID: "s", Buffer: []byte("x")}}}); err != nil {
		t.Fatal(err)
	}
	st, err := loadHandoffState(path)
	if err != nil || st.Version != 1 {
		t.Fatalf("v1 状态应可读取: st=%+v err=%v", st, err)
	}
}

func TestWithHandoffEnvReplacesOldState(t *testing.T) {
	env := []string{"PATH=/bin", handoffStateEnv + "=/old", "HOME=/tmp"}
	got := withHandoffEnv(env, "/new")
	want := []string{"PATH=/bin", "HOME=/tmp", handoffStateEnv + "=/new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("withHandoffEnv = %#v, want %#v", got, want)
	}
}

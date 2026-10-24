//go:build windows

package main

import (
	"testing"
	"unsafe"
)

func TestReadPseudoConsole(t *testing.T) {
	want := pseudoConsole{hSignal: 11, hPtyReference: 22, hConPtyProcess: 33}
	got, err := readPseudoConsole(uintptr(unsafe.Pointer(&want)))
	if err != nil {
		t.Fatalf("readPseudoConsole: %v", err)
	}
	if got != want {
		t.Fatalf("readPseudoConsole = %#v, want %#v", got, want)
	}
}

package main

import "testing"

func TestFreeUSBPortUsesMatchingHub(t *testing.T) {
	const status = "hub port sta spd dev sockfd local_busid\n" +
		"hs 0000 006 002 00010002 000005 1-1\n" +
		"ss 0008 004 000 00000000 000000 0-0\n" +
		"hs 0001 004 000 00000000 000000 0-0\n"
	for hub, want := range map[string]int32{"hs": 1, "ss": 8} {
		if port, ok := freeUSBPort(status, hub); !ok || port != want {
			t.Fatalf("%s port: %d, %t", hub, port, ok)
		}
	}
	if _, ok := freeUSBPort("hs 0000 006\nhs invalid 004\nss 0008 004", "hs"); ok {
		t.Fatal("accepted an occupied or invalid high-speed port")
	}
}

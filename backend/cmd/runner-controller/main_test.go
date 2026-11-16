package main

import "testing"

func TestStorageLimitBytes(t *testing.T) {
	for _, value := range []struct {
		input string
		want  int64
	}{
		{"0", 0},
		{"512", 512 << 20},
		{"4096", 4096 << 20},
		{"8796093022207", (1<<63 - 1) &^ (1<<20 - 1)},
	} {
		got, err := storageLimitBytes(value.input)
		if err != nil || got != value.want {
			t.Fatalf("%s: got %d, %v; want %d", value.input, got, err, value.want)
		}
	}
	for _, value := range []string{"", "-1", "1.5", "unlimited", "8796093022208", "18446744073709551616"} {
		if _, err := storageLimitBytes(value); err == nil {
			t.Fatalf("accepted invalid or overflowing storage limit %q", value)
		}
	}
}

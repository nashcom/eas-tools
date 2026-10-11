package certreload_test

import (
	"os"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/certreload"
)

// A pair that replaces another one with the same sizes and the same modification time is a change: time and size do not tell it, the content
// does. This is what happens when two pairs are written in the same instant, and when a swapped link points at a pair that looks the same.
func TestAReplacementWithTheSameSizeAndTimeIsSeen(t *testing.T) {
	f, first := setup(t, "one.example.com")
	s, err := certreload.New(f.cert, f.key, quiet())
	if err != nil {
		t.Fatal(err)
	}
	s.Settle = 10 * time.Millisecond

	oldCert, _ := os.Stat(f.cert)
	oldKey, _ := os.Stat(f.key)

	// another pair with the sizes of the first: the names have the same length, the sizes of the keys and the signatures vary by a byte or two,
	// so make pairs until one fits
	var c2, k2 []byte
	for i := 0; ; i++ {
		if i > 200 {
			t.Skip("no pair of the same size in 200 tries")
		}
		c, k, serial := valid(t, "two.example.com")
		if int64(len(c)) == oldCert.Size() && int64(len(k)) == oldKey.Size() && serial.Cmp(first) != 0 {
			c2, k2 = c, k
			break
		}
	}
	write(t, f, c2, k2)
	if err := os.Chtimes(f.cert, oldCert.ModTime(), oldCert.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f.key, oldKey.ModTime(), oldKey.ModTime()); err != nil {
		t.Fatal(err)
	}
	newCert, _ := os.Stat(f.cert)
	if newCert.Size() != oldCert.Size() || !newCert.ModTime().Equal(oldCert.ModTime()) {
		t.Fatal("the test did not make a replacement with the same size and time")
	}

	if !s.Check(t.Context()) {
		t.Error("the replacement with the same size and the same time was not seen")
	}
	if serialOf(t, s).Cmp(first) == 0 {
		t.Error("the old certificate is still served")
	}
}

package controller

import (
	"errors"
	"testing"
)

func TestErrText(t *testing.T) {
	const want = "the webhook failed"
	for _, text := range []string{want, want + "."} {
		if got := errText(errors.New(text)); got != want {
			t.Errorf("errText(%q) = %q, want %q", text, got, want)
		}
	}
}

package watcher

import "testing"

func TestSameText(t *testing.T) {
	if !SameText("a\r\nb", "a\nb") {
		t.Error("CRLF and LF should be the same text")
	}
	if SameText("a", "b") {
		t.Error("a and b differ")
	}
	if !SameText("", "") {
		t.Error("empty strings are the same")
	}
}

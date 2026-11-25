package transcript

import (
	"testing"
	"unicode/utf8"
)

func TestTokenMergingFinalAndNonFinal(t *testing.T) {
	var b Buffer

	// First partial update
	b.ApplyTokens([]Token{
		{Text: "Hello", IsFinal: false},
		{Text: " wor", IsFinal: false},
	})
	if got := b.FullText(); got != "Hello wor" {
		t.Fatalf("got %q, want %q", got, "Hello wor")
	}

	// Finalize "Hello ", new provisional "world"
	b.ApplyTokens([]Token{
		{Text: "Hello ", IsFinal: true},
		{Text: "world", IsFinal: false},
	})
	fin, nonFin := b.Parts()
	if fin != "Hello " || nonFin != "world" {
		t.Fatalf("parts = (%q, %q), want (%q, %q)", fin, nonFin, "Hello ", "world")
	}

	// Special markers (<end>, <fin>) must never appear in transcript
	b.ApplyTokens([]Token{
		{Text: "world!", IsFinal: true},
		{Text: "<end>", IsFinal: true},
		{Text: "<fin>", IsFinal: true},
		{Text: " Καλημέρα", IsFinal: false},
	})
	if got := b.FullText(); got != "Hello world! Καλημέρα" {
		t.Fatalf("got %q, want %q", got, "Hello world! Καλημέρα")
	}
}

func TestEditModeKeepsLiveSpeechAndMergesOnSubmit(t *testing.T) {
	var b Buffer
	b.ApplyTokens([]Token{
		{Text: "Καλησπέρα team, ", IsFinal: true},
		{Text: "testing", IsFinal: false},
	})

	editInitial := b.EnterEditMode()
	if editInitial != "Καλησπέρα team, testing" {
		t.Fatalf("editInitial = %q", editInitial)
	}

	// New speech arrives while user is editing!
	b.ApplyTokens([]Token{
		{Text: "και συνεχίζουμε", IsFinal: true},
		{Text: " live", IsFinal: false},
	})

	// User modifies text in edit box (say fixing a typo)
	userEdited := "Καλησπέρα team, fixed text."

	// Submit merges user edits + live post-edit final + non-final!
	got := b.ComposeFinal(userEdited)
	want := "Καλησπέρα team, fixed text. και συνεχίζουμε live"
	if got != want {
		t.Fatalf("ComposeFinal = %q, want %q", got, want)
	}
}

func TestEditModeDeduplicatesProvisionalFinalizationByTimestamp(t *testing.T) {
	var b Buffer
	b.ApplyTokens([]Token{
		{Text: "Hello ", IsFinal: true, StartMs: 0, EndMs: 400},
		{Text: "wor", IsFinal: false, StartMs: 400, EndMs: 700},
	})
	initText := b.EnterEditMode()
	if initText != "Hello wor" {
		t.Fatalf("initText = %q", initText)
	}

	// Soniox finalizes the SAME pre-edit word (StartMs=400 < editCutoffMs=700) AND emits new speech (StartMs=900)
	b.ApplyTokens([]Token{
		{Text: "world", IsFinal: true, StartMs: 400, EndMs: 750},
		{Text: " how are you", IsFinal: true, StartMs: 900, EndMs: 1600},
	})

	got := b.ComposeFinal("Hello world,")
	want := "Hello world, how are you"
	if got != want {
		t.Fatalf("ComposeFinal duplicated pre-edit provisional token: got %q, want %q", got, want)
	}
}

func TestCommitProvisionalAcrossReconnect(t *testing.T) {
	var b Buffer
	b.ApplyTokens([]Token{
		{Text: "First part ", IsFinal: true},
		{Text: "hello", IsFinal: false},
	})
	b.CommitProvisional()
	// New WebSocket sends "world" WITHOUT a leading space — boundary join must insert one space!
	b.ApplyTokens([]Token{
		{Text: "world", IsFinal: true},
	})
	if got := b.FullText(); got != "First part hello world" {
		t.Fatalf("after reconnect got %q, want %q", got, "First part hello world")
	}
}

func TestUnicodeSafeTrimmingGreekAndEmoji(t *testing.T) {
	mixed := "Καλημέρα κόσμε! Άλφα Βήτα Γάμμα 🎤🔥 Hello"
	for max := 1; max <= 35; max++ {
		tr := TruncateRunes(mixed, max)
		if !utf8.ValidString(tr) {
			t.Fatalf("TruncateRunes(%d) produced invalid UTF-8: %q", max, tr)
		}
		if len([]rune(tr)) > max {
			t.Fatalf("TruncateRunes(%d) exceeded max runes: %d (%q)", max, len([]rune(tr)), tr)
		}
		tl := TailRunes(mixed, max)
		if !utf8.ValidString(tl) {
			t.Fatalf("TailRunes(%d) produced invalid UTF-8: %q", max, tl)
		}
	}
}

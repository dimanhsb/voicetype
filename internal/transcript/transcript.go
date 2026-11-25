package transcript

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Token represents a single token returned by Soniox real-time STT.
type Token struct {
	Text        string `json:"text"`
	IsFinal     bool   `json:"is_final"`
	Language    string `json:"language,omitempty"`
	StartMs     int64  `json:"start_ms,omitempty"`
	EndMs       int64  `json:"end_ms,omitempty"`
	StartTimeMs int64  `json:"start_time_ms,omitempty"`
	DurationMs  int64  `json:"duration_ms,omitempty"`
}

func (t Token) startTime() int64 {
	if t.StartMs > 0 {
		return t.StartMs
	}
	return t.StartTimeMs
}

func (t Token) endTime() int64 {
	if t.EndMs > 0 {
		return t.EndMs
	}
	if t.StartTimeMs > 0 || t.DurationMs > 0 {
		return t.StartTimeMs + t.DurationMs
	}
	return 0
}

// IsControlToken reports whether text is a Soniox special marker token
// (such as <end> from endpoint detection or <fin> from manual finalization)
// that must never be inserted as spoken text.
func IsControlToken(text string) bool {
	trimmed := strings.TrimSpace(text)
	switch trimmed {
	case "<end>", "<fin>":
		return true
	}
	// Defensive check for any future angle-bracketed control markers like <spk> or <sil>
	if len(trimmed) >= 3 && trimmed[0] == '<' && trimmed[len(trimmed)-1] == '>' {
		inner := trimmed[1 : len(trimmed)-1]
		if len(inner) > 0 && len(inner) <= 16 {
			allLower := true
			for _, r := range inner {
				if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
					allLower = false
					break
				}
			}
			if allLower {
				return true
			}
		}
	}
	return false
}

// Buffer tracks confirmed (final) and provisional (non-final) text across
// normal recording, mid-dictation reconnects, and edit mode without losing speech.
type Buffer struct {
	// Normal recording state:
	finalText    strings.Builder
	nonFinalText string
	maxSeenEndMs int64
	joinBoundary bool // true immediately after reconnect so next stream joins with clean spacing

	// Edit mode state:
	// When editing is active, editBase holds the frozen text handed to the EDIT box,
	// while postEditFinal + postEditNonFinal accumulate any new speech spoken while editing.
	// editCutoffMs prevents provisional pre-edit words already frozen into editBase
	// from duplicating in the continuation strip when Soniox later finalizes them.
	editing          bool
	editBase         string
	editCutoffMs     int64
	postEditFinal    strings.Builder
	postEditNonFinal string
}

// Reset clears all accumulated transcript text and leaves edit mode.
func (b *Buffer) Reset() {
	b.finalText.Reset()
	b.nonFinalText = ""
	b.maxSeenEndMs = 0
	b.joinBoundary = false
	b.editing = false
	b.editBase = ""
	b.editCutoffMs = 0
	b.postEditFinal.Reset()
	b.postEditNonFinal = ""
}

func needsBoundarySpace(prev, next string) bool {
	if prev == "" || next == "" {
		return false
	}
	lastRune, _ := utf8.DecodeLastRuneInString(prev)
	firstRune, _ := utf8.DecodeRuneInString(next)
	if unicode.IsSpace(lastRune) || unicode.IsSpace(firstRune) || isAttachPunctuation(firstRune) {
		return false
	}
	return true
}

// ApplyTokens processes a batch of Soniox tokens from one WebSocket response message.
// Final tokens arrive once and are appended; non-final tokens replace the provisional tail.
// Special marker tokens (<end>, <fin>) are filtered out.
func (b *Buffer) ApplyTokens(tokens []Token) {
	var nonFinal strings.Builder
	for _, tok := range tokens {
		if tok.Text == "" || IsControlToken(tok.Text) {
			continue
		}
		if end := tok.endTime(); end > b.maxSeenEndMs {
			b.maxSeenEndMs = end
		}
		if b.editing && b.editCutoffMs > 0 {
			// Skip revisions/finalizations of pre-edit audio already frozen into the edit box!
			st := tok.startTime()
			end := tok.endTime()
			if (end > 0 && end <= b.editCutoffMs) || (st > 0 && st < b.editCutoffMs) {
				continue
			}
		}
		if tok.IsFinal {
			if b.editing {
				if b.joinBoundary && needsBoundarySpace(b.postEditFinal.String(), tok.Text) {
					b.postEditFinal.WriteByte(' ')
				}
				b.joinBoundary = false
				b.postEditFinal.WriteString(tok.Text)
			} else {
				if b.joinBoundary && needsBoundarySpace(b.finalText.String(), tok.Text) {
					b.finalText.WriteByte(' ')
				}
				b.joinBoundary = false
				b.finalText.WriteString(tok.Text)
			}
		} else {
			if b.joinBoundary && nonFinal.Len() == 0 {
				var prev string
				if b.editing {
					prev = b.postEditFinal.String()
				} else {
					prev = b.finalText.String()
				}
				if needsBoundarySpace(prev, tok.Text) {
					nonFinal.WriteByte(' ')
				}
			}
			nonFinal.WriteString(tok.Text)
		}
	}
	if b.editing {
		b.postEditNonFinal = nonFinal.String()
	} else {
		b.nonFinalText = nonFinal.String()
	}
}

// CommitProvisional promotes any current non-final tail into final text and resets
// the stream timestamp cutoff (because a new WebSocket stream starts timestamps at 0).
func (b *Buffer) CommitProvisional() {
	b.maxSeenEndMs = 0
	b.editCutoffMs = 0
	if b.editing {
		if b.postEditNonFinal != "" {
			b.postEditFinal.WriteString(b.postEditNonFinal)
			b.postEditNonFinal = ""
		}
		if b.postEditFinal.Len() > 0 {
			b.joinBoundary = true
		}
		return
	}
	if b.nonFinalText != "" {
		b.finalText.WriteString(b.nonFinalText)
		b.nonFinalText = ""
	}
	if b.finalText.Len() > 0 {
		b.joinBoundary = true
	}
}

// EnterEditMode freezes the current full transcript into editBase and routes
// subsequent incoming speech to the live post-edit continuation buffer.
// Returns the text to populate in the edit box.
func (b *Buffer) EnterEditMode() string {
	if b.editing {
		return b.editBase
	}
	current := b.FullText()
	b.editing = true
	b.editBase = current
	b.editCutoffMs = b.maxSeenEndMs
	b.postEditFinal.Reset()
	b.postEditNonFinal = ""
	return current
}

// IsEditing reports whether the buffer is currently in edit mode.
func (b *Buffer) IsEditing() bool {
	return b.editing
}

// Parts returns (finalText, nonFinalText) for normal mode rendering.
func (b *Buffer) Parts() (finalStr, nonFinalStr string) {
	return b.finalText.String(), b.nonFinalText
}

// PostEditParts returns (finalContinuation, nonFinalContinuation) spoken while in edit mode.
func (b *Buffer) PostEditParts() (finalStr, nonFinalStr string) {
	return b.postEditFinal.String(), b.postEditNonFinal
}

// FullText returns the complete current transcript in normal mode (final + non-final).
func (b *Buffer) FullText() string {
	if !b.editing {
		return b.finalText.String() + b.nonFinalText
	}
	return ComposeWithContinuation(b.editBase, b.postEditFinal.String()+b.postEditNonFinal)
}

// ComposeFinal combines the user's possibly modified edit-box text with any speech
// spoken during edit mode (final + non-final). In non-edit mode, returns FullText().
func (b *Buffer) ComposeFinal(editedBoxText string) string {
	if !b.editing {
		return strings.TrimSpace(b.finalText.String() + b.nonFinalText)
	}
	continuation := b.postEditFinal.String() + b.postEditNonFinal
	return strings.TrimSpace(ComposeWithContinuation(editedBoxText, continuation))
}

// ComposeWithContinuation joins two text fragments cleanly, inserting a single space
// only when both sides are non-empty and lack boundary whitespace (or leading punctuation).
func ComposeWithContinuation(base, continuation string) string {
	baseTrimRight := strings.TrimRightFunc(base, unicode.IsSpace)
	contTrimLeft := strings.TrimLeftFunc(continuation, unicode.IsSpace)
	if baseTrimRight == "" {
		return contTrimLeft
	}
	if contTrimLeft == "" {
		return baseTrimRight
	}
	firstRune, _ := utf8.DecodeRuneInString(contTrimLeft)
	if isAttachPunctuation(firstRune) {
		return baseTrimRight + contTrimLeft
	}
	return baseTrimRight + " " + contTrimLeft
}

func isAttachPunctuation(r rune) bool {
	switch r {
	case '.', ',', '!', '?', ';', ':', ')', ']', '}', '»', '”', '’', '·', ';':
		return true
	default:
		return false
	}
}

// TruncateRunes truncates s to at most maxRunes Unicode code points (never splitting
// a multi-byte UTF-8 character like Greek letters or emoji), appending "…" if truncated.
func TruncateRunes(s string, maxRunes int) string {
	s = CollapseWhitespace(s)
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes == 1 {
		return "…"
	}
	return string(runes[:maxRunes-1]) + "…"
}

// TailRunes returns at most the last maxRunes Unicode code points of s (safe for Greek/emoji).
func TailRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[len(runes)-maxRunes:])
}

// CollapseWhitespace replaces newlines and runs of whitespace with a single space (for tray menu labels).
func CollapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

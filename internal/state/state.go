package state

import "fmt"

// State represents the explicit lifecycle state of a dictation session.
type State int

const (
	Idle State = iota
	Connecting
	Recording
	Editing
	Reconnecting
	Inserting
	Error
)

func (s State) String() string {
	switch s {
	case Idle:
		return "Idle"
	case Connecting:
		return "Connecting"
	case Recording:
		return "Recording"
	case Editing:
		return "Editing"
	case Reconnecting:
		return "Reconnecting"
	case Inserting:
		return "Inserting"
	case Error:
		return "Error"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// IsActive reports whether a dictation session is currently open (capturing/streaming/editing).
func (s State) IsActive() bool {
	return s == Connecting || s == Recording || s == Editing || s == Reconnecting
}

// Action is a side effect requested by a state machine transition.
type Action int

const (
	ActionNone Action = iota
	ActionStartSession      // Capture target HWND, start buffering audio, dial Soniox
	ActionEnterEditMode     // Show focused edit box, keep audio + Soniox streaming
	ActionClearAndRestart   // Clear transcript, hide edit box, keep recording fresh
	ActionSubmit            // Close Soniox immediately, insert current text
	ActionStartReconnect    // Commit provisional text, dial new Soniox socket with backoff
	ActionCancelToIdle      // Close Soniox, stop buffering, hide overlay
	ActionShowError         // Show visible error state in overlay
	ActionFinishInsert      // Return to Idle after insertion completes
)

// Transition describes the result of handling an event.
type Transition struct {
	From      State
	To        State
	SessionID uint64
	Action    Action
	Ignored   bool
	Reason    string
}

// Machine enforces valid state transitions and monotonic session IDs so stale async
// callbacks (e.g. a late socket close or rapid hotkey presses) cannot wedge the app.
type Machine struct {
	state         State
	sessionID     uint64
	wasEditing    bool // remembers if we were in Editing when a reconnect started
	clearedOnce   bool // true right after 2nd Esc (Clear & Restart); a 3rd Esc exits to Idle!
	lastErrorText string
}

// New creates a state machine starting in Idle.
func New() *Machine {
	return &Machine{state: Idle}
}

func (m *Machine) State() State       { return m.state }
func (m *Machine) SessionID() uint64  { return m.sessionID }
func (m *Machine) LastError() string  { return m.lastErrorText }
func (m *Machine) WasEditing() bool   { return m.wasEditing }
func (m *Machine) ClearedOnce() bool  { return m.clearedOnce }

// OnSpeechReceived clears the post-clear flag once the user starts dictating new words
// after pressing Esc twice, so pressing Esc on the new dictation opens Edit Mode again.
func (m *Machine) OnSpeechReceived() {
	m.clearedOnce = false
}

// OnHotkeyDown handles pressing the global dictation hotkey (default F9).
// In toggle mode: Idle/Error -> Connecting; Active -> Inserting (submit).
// In push-to-talk mode: Idle/Error -> Connecting; Active -> ignored (release submits).
func (m *Machine) OnHotkeyDown(pushToTalk bool) Transition {
	from := m.state
	switch m.state {
	case Idle, Error:
		m.sessionID++
		m.state = Connecting
		m.wasEditing = false
		m.clearedOnce = false
		m.lastErrorText = ""
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Action: ActionStartSession}
	case Connecting, Recording, Editing, Reconnecting:
		if pushToTalk {
			return Transition{From: from, To: m.state, SessionID: m.sessionID, Ignored: true, Reason: "ptt key repeat"}
		}
		m.state = Inserting
		m.wasEditing = false
		m.clearedOnce = false
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Action: ActionSubmit}
	case Inserting:
		// Ignore hotkey while inserting so rapid double-taps cannot race
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Ignored: true, Reason: "inserting in progress"}
	default:
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Ignored: true}
	}
}

// OnHotkeyUp handles releasing the global hotkey (only acts in push-to-talk mode).
func (m *Machine) OnHotkeyUp(pushToTalk bool) Transition {
	from := m.state
	if !pushToTalk {
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Ignored: true}
	}
	switch m.state {
	case Connecting, Recording:
		m.state = Inserting
		m.wasEditing = false
		m.clearedOnce = false
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Action: ActionSubmit}
	case Reconnecting:
		if m.wasEditing {
			// User entered edit mode before/during reconnect; do not submit on PTT release.
			return Transition{From: from, To: m.state, SessionID: m.sessionID, Ignored: true, Reason: "editing active during reconnect"}
		}
		m.state = Inserting
		m.wasEditing = false
		m.clearedOnce = false
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Action: ActionSubmit}
	case Editing:
		// If the user explicitly pressed Esc to enter edit mode during PTT, let them finish editing
		// and submit with Enter/F9 instead of aborting on key release.
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Ignored: true, Reason: "editing active"}
	default:
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Ignored: true}
	}
}

// OnEnter handles pressing Enter while active: immediately submits whatever text exists now.
func (m *Machine) OnEnter() Transition {
	from := m.state
	if !m.state.IsActive() {
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Ignored: true}
	}
	m.state = Inserting
	m.wasEditing = false
	m.clearedOnce = false
	return Transition{From: from, To: m.state, SessionID: m.sessionID, Action: ActionSubmit}
}

// OnEscape handles pressing Esc while active:
// - 1st Esc (Connecting/Recording/Reconnecting): enters Editing mode (recording continues).
// - 2nd Esc (Editing, or Reconnecting while editing): clears everything and restarts Recording fresh.
// - 3rd Esc (after clearing with no new speech): exits listening mode completely -> Idle (no insert).
// - From Error: dismisses back to Idle.
func (m *Machine) OnEscape() Transition {
	from := m.state
	switch m.state {
	case Connecting, Recording:
		if m.clearedOnce {
			m.sessionID++ // invalidate any in-flight callbacks for the cancelled session
			m.state = Idle
			m.wasEditing = false
			m.clearedOnce = false
			m.lastErrorText = ""
			return Transition{From: from, To: Idle, SessionID: m.sessionID, Action: ActionCancelToIdle}
		}
		m.state = Editing
		m.wasEditing = true
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Action: ActionEnterEditMode}
	case Reconnecting:
		if m.wasEditing {
			// Already editing during reconnect: Esc acts as 2nd Esc (clear & restart) instead of clobbering edits!
			m.state = Recording
			m.wasEditing = false
			m.clearedOnce = true
			return Transition{From: from, To: m.state, SessionID: m.sessionID, Action: ActionClearAndRestart}
		}
		if m.clearedOnce {
			m.sessionID++
			m.state = Idle
			m.wasEditing = false
			m.clearedOnce = false
			m.lastErrorText = ""
			return Transition{From: from, To: Idle, SessionID: m.sessionID, Action: ActionCancelToIdle}
		}
		m.state = Editing
		m.wasEditing = true
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Action: ActionEnterEditMode}
	case Editing:
		m.state = Recording
		m.wasEditing = false
		m.clearedOnce = true
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Action: ActionClearAndRestart}
	case Error:
		m.state = Idle
		m.clearedOnce = false
		m.lastErrorText = ""
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Action: ActionCancelToIdle}
	default:
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Ignored: true}
	}
}

// OnConnected handles WebSocket connection + config ack for the given sessionID.
func (m *Machine) OnConnected(sessionID uint64) Transition {
	from := m.state
	if sessionID != m.sessionID {
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Ignored: true, Reason: "stale session"}
	}
	switch m.state {
	case Connecting:
		m.state = Recording
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Action: ActionNone}
	case Reconnecting:
		if m.wasEditing {
			m.state = Editing
		} else {
			m.state = Recording
		}
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Action: ActionNone}
	case Editing:
		// Already entered edit mode while initial connect was in flight — stay in Editing!
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Action: ActionNone}
	default:
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Ignored: true}
	}
}

// OnConnectionLost handles an unexpected WebSocket drop mid-dictation.
// If fatal is true (e.g. invalid API key 401/403), transitions to Error without infinite retries.
func (m *Machine) OnConnectionLost(sessionID uint64, errMsg string, fatal bool) Transition {
	from := m.state
	if sessionID != m.sessionID {
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Ignored: true, Reason: "stale session"}
	}
	if !m.state.IsActive() {
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Ignored: true}
	}
	m.lastErrorText = errMsg
	if fatal {
		m.state = Error
		m.wasEditing = false
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Action: ActionShowError}
	}
	if m.state == Editing {
		m.wasEditing = true
	}
	m.state = Reconnecting
	return Transition{From: from, To: m.state, SessionID: m.sessionID, Action: ActionStartReconnect}
}

// OnInsertComplete transitions from Inserting back to Idle for the matching sessionID.
func (m *Machine) OnInsertComplete(sessionID uint64) Transition {
	from := m.state
	if sessionID != m.sessionID || m.state != Inserting {
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Ignored: true}
	}
	m.state = Idle
	m.wasEditing = false
	m.lastErrorText = ""
	return Transition{From: from, To: m.state, SessionID: m.sessionID, Action: ActionFinishInsert}
}

// OnErrorDismissed transitions from Error back to Idle (e.g. after auto-hide timer).
func (m *Machine) OnErrorDismissed(sessionID uint64) Transition {
	from := m.state
	if sessionID != m.sessionID || m.state != Error {
		return Transition{From: from, To: m.state, SessionID: m.sessionID, Ignored: true}
	}
	m.state = Idle
	m.lastErrorText = ""
	return Transition{From: from, To: m.state, SessionID: m.sessionID, Action: ActionCancelToIdle}
}

// OnCancel forces any active or error state back to Idle (e.g. on shutdown).
func (m *Machine) OnCancel() Transition {
	from := m.state
	if m.state == Idle {
		return Transition{From: from, To: Idle, SessionID: m.sessionID, Ignored: true}
	}
	m.sessionID++ // invalidate any in-flight callbacks
	m.state = Idle
	m.wasEditing = false
	m.clearedOnce = false
	m.lastErrorText = ""
	return Transition{From: from, To: Idle, SessionID: m.sessionID, Action: ActionCancelToIdle}
}

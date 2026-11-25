package state

import "testing"

func TestBasicDictationLifecycleAndRapidDoubleF9(t *testing.T) {
	m := New()
	if m.State() != Idle {
		t.Fatalf("initial state = %s, want Idle", m.State())
	}

	// 1st F9 -> Connecting
	t1 := m.OnHotkeyDown(false)
	if t1.To != Connecting || t1.Action != ActionStartSession || t1.SessionID != 1 {
		t.Fatalf("unexpected t1: %+v", t1)
	}

	// Connected -> Recording
	t2 := m.OnConnected(1)
	if t2.To != Recording || t2.Ignored {
		t.Fatalf("unexpected t2: %+v", t2)
	}

	// 2nd F9 -> Inserting (immediate submit!)
	t3 := m.OnHotkeyDown(false)
	if t3.To != Inserting || t3.Action != ActionSubmit {
		t.Fatalf("unexpected t3: %+v", t3)
	}

	// Rapid extra F9 while Inserting must be ignored, not wedge!
	tSpam := m.OnHotkeyDown(false)
	if !tSpam.Ignored || m.State() != Inserting {
		t.Fatalf("spam F9 during Inserting should be ignored: %+v", tSpam)
	}

	// Insert completes -> Idle
	t4 := m.OnInsertComplete(1)
	if t4.To != Idle || t4.Ignored {
		t.Fatalf("unexpected t4: %+v", t4)
	}

	// Next session works cleanly (sessionID = 2)
	t5 := m.OnHotkeyDown(false)
	if t5.To != Connecting || t5.SessionID != 2 {
		t.Fatalf("second session failed: %+v", t5)
	}
	// Stale callback from session 1 is ignored!
	stale := m.OnConnected(1)
	if !stale.Ignored || m.State() != Connecting {
		t.Fatalf("stale callback should be ignored: %+v", stale)
	}
}

func TestEscapeOnceEditEscapeTwiceClear(t *testing.T) {
	m := New()
	m.OnHotkeyDown(false)
	m.OnConnected(1)

	// Esc once -> Editing
	e1 := m.OnEscape()
	if e1.To != Editing || e1.Action != ActionEnterEditMode {
		t.Fatalf("1st Esc = %+v", e1)
	}

	// Connection drop while Editing -> Reconnecting -> Connected returns to Editing!
	drop := m.OnConnectionLost(1, "network blip", false)
	if drop.To != Reconnecting || drop.Action != ActionStartReconnect {
		t.Fatalf("drop in editing = %+v", drop)
	}
	rec := m.OnConnected(1)
	if rec.To != Editing {
		t.Fatalf("reconnect should restore Editing, got %s", rec.To)
	}

	// Esc twice -> ClearAndRestart back to Recording (clearedOnce=true)
	e2 := m.OnEscape()
	if e2.To != Recording || e2.Action != ActionClearAndRestart || !m.ClearedOnce() {
		t.Fatalf("2nd Esc = %+v, clearedOnce=%v", e2, m.ClearedOnce())
	}

	// Esc a 3rd time (before speaking new words) -> exits completely to Idle!
	e3 := m.OnEscape()
	if e3.To != Idle || e3.Action != ActionCancelToIdle || m.ClearedOnce() {
		t.Fatalf("3rd Esc should exit to Idle, got %+v", e3)
	}

	// Verify speaking new words after 2nd Esc resets clearedOnce so Esc enters Edit Mode again!
	m.OnHotkeyDown(false)
	m.OnConnected(m.SessionID())
	m.OnEscape()         // 1st Esc -> Editing
	m.OnEscape()         // 2nd Esc -> Clear & Restart (clearedOnce=true)
	m.OnSpeechReceived() // User speaks new words -> clearedOnce=false
	eAfterSpeech := m.OnEscape()
	if eAfterSpeech.To != Editing || eAfterSpeech.Action != ActionEnterEditMode {
		t.Fatalf("Esc after speaking new words should enter Edit Mode, got %+v", eAfterSpeech)
	}

	// Verify Esc & PTT release during edit-mode reconnect do NOT overwrite edits or prematurely submit!
	m.OnConnectionLost(m.SessionID(), "drop2", false)
	if m.State() != Reconnecting || !m.WasEditing() {
		t.Fatalf("expected Reconnecting with wasEditing=true, got state=%s wasEditing=%v", m.State(), m.WasEditing())
	}
	pttUp := m.OnHotkeyUp(true)
	if !pttUp.Ignored {
		t.Fatalf("PTT release during edit-mode reconnect should be ignored: %+v", pttUp)
	}
	escDuringRec := m.OnEscape()
	if escDuringRec.Action != ActionClearAndRestart || escDuringRec.To != Recording {
		t.Fatalf("Esc during edit-mode reconnect should clear & restart, not re-enter edit: %+v", escDuringRec)
	}

	// Enter submits immediately
	sub := m.OnEnter()
	if sub.To != Inserting || sub.Action != ActionSubmit {
		t.Fatalf("Enter = %+v", sub)
	}
}

func TestPushToTalkAndFatalError(t *testing.T) {
	m := New()
	// PTT press -> Connecting
	tr := m.OnHotkeyDown(true)
	if tr.To != Connecting {
		t.Fatalf("PTT down = %+v", tr)
	}
	// Key repeat ignored
	rep := m.OnHotkeyDown(true)
	if !rep.Ignored {
		t.Fatalf("PTT repeat should be ignored")
	}
	// Release even while Connecting submits immediately!
	up := m.OnHotkeyUp(true)
	if up.To != Inserting || up.Action != ActionSubmit {
		t.Fatalf("PTT up = %+v", up)
	}
	m.OnInsertComplete(1)

	// Fatal error -> Error -> Esc returns to Idle
	m.OnHotkeyDown(false)
	fe := m.OnConnectionLost(2, "401 unauthorized", true)
	if fe.To != Error || fe.Action != ActionShowError {
		t.Fatalf("fatal error = %+v", fe)
	}
	esc := m.OnEscape()
	if esc.To != Idle {
		t.Fatalf("Esc from Error should go to Idle, got %s", esc.To)
	}
}

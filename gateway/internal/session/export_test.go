package session

// SetCloseHook installs the hook CloseTouched runs between its snapshot of a session's children and its closing of them.
func SetCloseHook(f func(string)) { closeHook = f }

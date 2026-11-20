package devicestate

import (
	"fmt"
	"runtime/debug"
)

// PanicError is what a recovered panic of a goroutine of the snapshot engine is reported as.
type PanicError struct {
	Name  string // the goroutine
	Value any
	Stack []byte
}

func (e *PanicError) Error() string { return fmt.Sprintf("panic in %s: %v", e.Name, e.Value) }

// safeGo runs fn (the body of a goroutine: write `go safeGo(...)`) and turns a panic into a call of onPanic, so that one bad device or layer
// cannot take the whole node-state pod down with the snapshots of every other device. onPanic may be nil. The stack is in the PanicError.
func safeGo(name string, onPanic func(error), fn func()) {
	defer func() {
		if v := recover(); v != nil {
			if onPanic != nil {
				onPanic(&PanicError{Name: name, Value: v, Stack: debug.Stack()})
			}
		}
	}()
	fn()
}

package progress

type discardCounter struct{}

func (discardCounter) Add(int64) {}
func (discardCounter) Finish()   {}
func (discardCounter) Abort()    {}

// Discard is a Counter that ignores all updates.
var Discard Counter = discardCounter{}

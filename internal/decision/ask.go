package decision

import "context"

// Asker is supplied only by an interactive transport, never by approval policy.
type Asker func(context.Context, Question) (Answer, error)

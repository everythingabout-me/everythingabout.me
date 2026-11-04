package filechanges

import "context"

type attributionKey struct{}
type attribution struct {
	actor     Actor
	requestID string
}

// WithActor carries a captured, non-secret request identity into synchronous
// settlement. Async operations must persist their attribution in the journal;
// context cancellation or worker replacement does not reconstruct the actor.
func WithActor(ctx context.Context, actor Actor, requestID string) context.Context {
	return context.WithValue(ctx, attributionKey{}, attribution{actor: actor, requestID: requestID})
}

func ActorFromContext(ctx context.Context) (Actor, string, bool) {
	value, ok := ctx.Value(attributionKey{}).(attribution)
	return value.actor, value.requestID, ok
}

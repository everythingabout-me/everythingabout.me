package filechanges

import (
	"context"
	"testing"
)

func TestCapturedActorContextIsolation(t *testing.T) {
	base := context.Background()
	if _, _, ok := ActorFromContext(base); ok {
		t.Fatal("empty context gained attribution")
	}
	actor := Actor{Kind: "token", PrincipalID: "token:stored-uuid", Label: "saved label", Scope: "/Apps/Notes", PolicyGeneration: "policy-3"}
	ctx := WithActor(base, actor, "req-captured")
	got, request, ok := ActorFromContext(ctx)
	if !ok || got != actor || request != "req-captured" {
		t.Fatalf("captured actor %+v request %q present %v", got, request, ok)
	}
	if _, _, ok := ActorFromContext(base); ok {
		t.Fatal("attribution escaped its request context")
	}
	got.Label = "renamed later"
	again, _, _ := ActorFromContext(ctx)
	if again.Label != actor.Label {
		t.Fatal("retrieved attribution mutated the captured value")
	}
}

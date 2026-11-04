package ai

import (
	"testing"
)

func TestConversationSubjectKeySeparatesExplicitThreads(t *testing.T) {
	context := ChatContext{Pathname: "/workspace", ShellMode: "workspace"}
	legacy := ConversationSubjectKey("alice", context)
	first := ConversationSubjectKey("alice", ChatContext{Pathname: context.Pathname, ShellMode: context.ShellMode, ThreadID: "thread-one"})
	second := ConversationSubjectKey("alice", ChatContext{Pathname: context.Pathname, ShellMode: context.ShellMode, ThreadID: "thread-two"})
	otherUser := ConversationSubjectKey("bob", ChatContext{Pathname: context.Pathname, ShellMode: context.ShellMode, ThreadID: "thread-one"})

	if legacy != "alice:/workspace::" {
		t.Fatalf("legacy thread key changed: %q", legacy)
	}
	if first == second || first == legacy || first == otherUser {
		t.Fatalf("thread or user histories are not isolated: legacy=%q first=%q second=%q other=%q", legacy, first, second, otherUser)
	}
	if len(first) > 80 {
		t.Fatalf("thread key is not bounded for the database column: length=%d", len(first))
	}
}

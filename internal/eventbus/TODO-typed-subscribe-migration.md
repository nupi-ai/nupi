# TODO: Migrate test Subscribe calls to typed API

## Context

`bus.Publish` was made private to enforce typed `Publish[T]` usage.
Production code already uses `Subscribe[T]` / `SubscribeTo[T]` everywhere.
Test files still use raw `bus.Subscribe()` + manual type assertion.

## Scope

~148 calls across 16 test files (excluding `bus_test.go` which tests raw API intentionally).

## Files to migrate

| File | Count |
|------|-------|
| internal/integration/voice_pipeline_integration_test.go | 56 |
| internal/intentrouter/service_test.go | 22 |
| internal/integration/session_intent_routing_integration_test.go | 9 |
| internal/audio/stt/service_test.go | 8 |
| internal/conversation/service_test.go | 7 |
| internal/contentpipeline/service_test.go | 6 |
| internal/plugins/adapters/log_writer_test.go | 6 |
| internal/server/handlers_test.go | 5 |
| internal/integration/session_voice_integration_test.go | 5 |
| internal/audio/barge/service_test.go | 4 |
| internal/audio/ingress/service_test.go | 4 |
| internal/audio/vad/service_test.go | 4 |
| internal/audio/egress/service_test.go | 4 |
| internal/plugins/adapters/service_test.go | 4 |
| internal/server/grpc_services_test.go | 2 |
| internal/plugins/adapters/manager_integration_test.go | 2 |

## Migration pattern

Before:
```go
sub := bus.Subscribe(eventbus.TopicSpeechVADDetected)
defer sub.Close()
// ...
select {
case env := <-sub.C():
    event, ok := env.Payload.(eventbus.SpeechVADEvent)
    if !ok {
        t.Fatalf("expected SpeechVADEvent, got %T", env.Payload)
    }
    // use event
}
```

After:
```go
sub := eventbus.SubscribeTo(bus, eventbus.Speech.VADDetected)
defer sub.Close()
// ...
select {
case env := <-sub.C():
    // env.Payload is already SpeechVADEvent, no assertion needed
    // use env.Payload directly
}
```

## Notes

- `bus_test.go` (9 calls) stays on raw API — tests raw bus mechanics intentionally
- Each migration eliminates manual type assertion boilerplate
- TypedSubscription bridge auto-logs mismatches via bus logger

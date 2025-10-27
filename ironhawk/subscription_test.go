package ironhawk

import (
    "reflect"
    "testing"

    "github.com/dicedb/dicedb-go/wire"
)

// fakeClient implements fireClient for testing command composition
type fakeClient struct{
    lastCmd *wire.Command
    res     *wire.Result
}

func (f *fakeClient) Fire(c *wire.Command) *wire.Result {
    f.lastCmd = c
    if f.res == nil {
        return &wire.Result{Status: wire.Status_OK}
    }
    return f.res
}

func TestExtractClientID_FromMessageJSON(t *testing.T) {
    resp := &wire.Result{
        Status:  wire.Status_OK,
        Message: `{"clientId":"abc123"}`,
    }
    got, ok := extractClientID(resp)
    if !ok || got != "abc123" {
        t.Fatalf("expected clientId abc123, got %q ok=%v", got, ok)
    }
}

func TestExtractClientID_FromMessageKV(t *testing.T) {
    cases := []string{
        "client_id: abc123",
        "clientId=abc123",
        "prefix client_id=abc123 suffix",
    }
    for _, msg := range cases {
        resp := &wire.Result{Status: wire.Status_OK, Message: msg}
        got, ok := extractClientID(resp)
        if !ok || got != "abc123" {
            t.Fatalf("message %q: expected abc123, got %q ok=%v", msg, got, ok)
        }
    }
}

func TestEmitReconnect_OKNextIndex(t *testing.T) {
    fc := &fakeClient{res: &wire.Result{Status: wire.Status_OK, Message: "OK 42"}}
    status, next, err := emitReconnect(fc, "k1", "cid:123", 17)
    if err != nil { t.Fatalf("unexpected error: %v", err) }
    if status != "OK" || next != 42 {
        t.Fatalf("expected OK 42, got %s %d", status, next)
    }
}

func TestEmitReconnect_TextStatuses(t *testing.T) {
    texts := []string{"STALE_SEQUENCE", "INVALID_SEQUENCE", "SUBSCRIPTION_NOT_FOUND"}
    for _, txt := range texts {
        fc := &fakeClient{res: &wire.Result{Status: wire.Status_ERR, Message: txt}}
        status, next, err := emitReconnect(fc, "k1", "cid:123", 9)
        if err != nil { t.Fatalf("%s: unexpected error: %v", txt, err) }
        if status != txt || next != 0 { t.Fatalf("%s: expected status passthrough, got %s %d", txt, status, next) }
    }
}

func TestSendEmitAck_ComposesCommand(t *testing.T) {
    fc := &fakeClient{}
    err := sendEmitAck(fc, "k1", "cid:123", 77)
    if err != nil { t.Fatalf("unexpected error: %v", err) }
    want := &wire.Command{Cmd: "EMITACK", Args: []string{"k1", "cid:123", "77"}}
    if fc.lastCmd == nil || fc.lastCmd.Cmd != want.Cmd || !reflect.DeepEqual(fc.lastCmd.Args, want.Args) {
        t.Fatalf("command mismatch: got %#v, want %#v", fc.lastCmd, want)
    }
}

// NOTE: Ack batching behavior is exercised indirectly in maybeAckOnReceive,
// which depends on extracting commitIndex from typed watch responses.
// Once the server exposes commitIndex in watch payloads (e.g., via an EmitSeq
// message), add fixtures here to assert batch flush on size and interval.
func TestMaybeAckOnReceive_Batching_Scaffold(t *testing.T) {
    t.Skip("requires a watch response fixture with commitIndex in typed payload; add once proto is available")
}

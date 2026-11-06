package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// Regression tests for the durable-outbox replay bug (CORE-1): the params map was
// stored from gotgbot's typed values, so numeric fields are JSON numbers. Decoding
// them without json.Decoder.UseNumber produced float64, gotgbot rendered the float
// in scientific notation, and every retried send failed with "chat not found" —
// i.e. the retry queue silently destroyed exactly the messages it existed to save.

// TestDecodeOutboxParamsKeepsNumbers: the decoded value must keep the original
// digits, and must NOT be a float64.
func TestDecodeOutboxParamsKeepsNumbers(t *testing.T) {
	raw := []byte(`{"chat_id":1234567890123,"message_id":42,"text":"hello"}`)
	params, err := decodeOutboxParams(raw)
	if err != nil {
		t.Fatalf("decodeOutboxParams: %v", err)
	}
	if _, isFloat := params["chat_id"].(float64); isFloat {
		t.Fatal("chat_id decoded as float64 — the scientific-notation bug is back")
	}
	if got := fmt.Sprint(params["chat_id"]); got != "1234567890123" {
		t.Errorf("rendered chat_id = %q; want the exact digits %q", got, "1234567890123")
	}
	if got := fmt.Sprint(params["message_id"]); got != "42" {
		t.Errorf("rendered message_id = %q; want 42", got)
	}
}

// TestDecodedOutboxParamsRenderThroughClient drives the decoded params through the
// REAL gotgbot client against the fake Bot API, which is where the float64 became
// "1.23456789e+09": this asserts the wire value Telegram would see.
func TestDecodedOutboxParamsRenderThroughClient(t *testing.T) {
	fake := newFakeTG()
	defer fake.srv.Close()

	tg, err := gotgbot.NewBot("123456:TESTTOKEN", &gotgbot.BotOpts{
		BotClient: &gotgbot.BaseBotClient{
			DefaultRequestOpts: &gotgbot.RequestOpts{APIURL: fake.srv.URL},
		},
	})
	if err != nil {
		t.Fatalf("new bot: %v", err)
	}

	raw := []byte(`{"chat_id":1234567890123,"message_id":7,"text":"retry me"}`)
	params, err := decodeOutboxParams(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := tg.RequestWithContext(context.Background(), "sendMessage", params, nil); err != nil {
		t.Fatalf("request: %v", err)
	}

	var sent map[string]string
	for _, c := range fake.since(0) {
		if c.method == "sendMessage" {
			sent = c.params
		}
	}
	if sent == nil {
		t.Fatalf("no sendMessage call recorded: %+v", fake.since(0))
	}
	if got := sent["chat_id"]; got != "1234567890123" {
		t.Errorf("wire chat_id = %q; want 1234567890123 (no scientific notation)", got)
	}
	if got := sent["text"]; got != "retry me" {
		t.Errorf("wire text = %q; want %q", got, "retry me")
	}
}

// TestDecodedOutboxParamsArrays: array params (copyMessages' message_ids) must
// re-marshal as exact integers too.
func TestDecodedOutboxParamsArrays(t *testing.T) {
	raw := []byte(`{"chat_id":99,"from_chat_id":98,"message_ids":[1234567890123,7]}`)
	params, err := decodeOutboxParams(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	b, err := json.Marshal(params["message_ids"])
	if err != nil {
		t.Fatalf("re-marshal message_ids: %v", err)
	}
	if got, want := string(b), "[1234567890123,7]"; got != want {
		t.Errorf("message_ids re-marshaled as %s; want %s", got, want)
	}
}

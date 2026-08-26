package bot

import (
	"testing"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// The chat gate in prep decides, before any handler runs, whose updates the bot
// is willing to act on. Getting it wrong is not a cosmetic bug: letting a chat
// through means the catch-all answers strangers, and rejecting one means the bot
// goes silent for real users.
func TestIgnoredChat(t *testing.T) {
	const gm = "-1001111111111"

	tests := []struct {
		name string
		chat *gotgbot.Chat
		want bool
	}{
		{
			name: "private chat is the normal case",
			chat: &gotgbot.Chat{Id: 42, Type: "private"},
			want: false,
		},
		{
			name: "ordinary supergroup stays allowed (Python parity)",
			chat: &gotgbot.Chat{Id: -100222, Type: "supergroup"},
			want: false, // deliberately allowed
		},
		{
			name: "legacy group rejected",
			chat: &gotgbot.Chat{Id: -222, Type: "group"},
			want: true,
		},
		{
			name: "channel rejected",
			chat: &gotgbot.Chat{Id: -100333, Type: "channel"},
			want: true,
		},
		{
			name: "GM group rejected by id",
			chat: &gotgbot.Chat{Id: -1001111111111, Type: "supergroup"},
			want: true,
		},
		{
			// The regression this gate was added for: a channel's direct-messages
			// chat is a supergroup, so without the is_direct_messages check it
			// would fall through the supergroup allowance into the catch-all.
			name: "channel direct-messages chat rejected",
			chat: &gotgbot.Chat{Id: -100444, Type: "supergroup", IsDirectMessages: true},
			want: true,
		},
		{
			// Belt and braces: the flag alone must be enough, whatever type
			// Telegram labels the chat with in future.
			name: "direct-messages chat rejected regardless of type",
			chat: &gotgbot.Chat{Id: -100555, Type: "private", IsDirectMessages: true},
			want: true,
		},
		{
			name: "nil chat is not treated as ignorable",
			chat: nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ignoredChat(tt.chat, gm); got != tt.want {
				t.Errorf("ignoredChat() = %v, want %v", got, tt.want)
			}
		})
	}
}

// An empty GM_GROUP_ID must not turn into a wildcard that rejects every chat
// whose id formats to "".
func TestIgnoredChatEmptyGMGroup(t *testing.T) {
	if ignoredChat(&gotgbot.Chat{Id: 42, Type: "private"}, "") {
		t.Error("a private chat must be allowed when GM_GROUP_ID is unset")
	}
}

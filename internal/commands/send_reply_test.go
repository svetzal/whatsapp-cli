package commands

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vicentereig/whatsapp-cli/internal/store"
	"github.com/vicentereig/whatsapp-cli/internal/types"
)

// TestSendReply_QuotesTheStoredMessage verifies a reply carries the quoted
// message's id, sender, and text, so the recipient's client renders the quote
// against the right message.
func TestSendReply_QuotesTheStoredMessage(t *testing.T) {
	chatJID := "919916556163@s.whatsapp.net"

	var captured types.QuotedContext
	client := &MockWAClient{
		SendReplyFunc: func(_ context.Context, _, _ string, quoted types.QuotedContext) (string, error) {
			captured = quoted
			return "sent-id", nil
		},
	}
	st := &MockMessageStore{
		GetMessageForDownloadFunc: func(id string, _ *string) (store.MessageDownloadInfo, error) {
			return store.MessageDownloadInfo{
				ID:       id,
				ChatJID:  chatJID,
				Sender:   chatJID,
				Content:  "We got this new error when customer was making a payment",
				IsFromMe: false,
			}, nil
		},
	}

	app := NewAppWithDeps(client, st, t.TempDir(), "test")
	result := app.SendReply(context.Background(), chatJID, "That was our fault.", "QUOTED-1")

	resp := parseResponse(t, result)
	require.True(t, resp.Success, "reply should succeed: %s", result)

	require.Equal(t, "QUOTED-1", captured.ID)
	require.Equal(t, chatJID, captured.Sender)
	require.Equal(t, "We got this new error when customer was making a payment", captured.Text)
}

// TestSendReply_QuotingOurOwnMessageUsesOurJID verifies the "me" sender the
// store records for outbound messages is replaced with an addressable JID.
func TestSendReply_QuotingOurOwnMessageUsesOurJID(t *testing.T) {
	var captured types.QuotedContext
	client := &MockWAClient{
		OwnJIDFunc: func() string { return "15559990000@s.whatsapp.net" },
		SendReplyFunc: func(_ context.Context, _, _ string, quoted types.QuotedContext) (string, error) {
			captured = quoted
			return "sent-id", nil
		},
	}
	st := &MockMessageStore{
		GetMessageForDownloadFunc: func(id string, _ *string) (store.MessageDownloadInfo, error) {
			return store.MessageDownloadInfo{
				ID:       id,
				Sender:   "me",
				Content:  "earlier note",
				IsFromMe: true,
			}, nil
		},
	}

	app := NewAppWithDeps(client, st, t.TempDir(), "test")
	result := app.SendReply(context.Background(), "someone@s.whatsapp.net", "following up", "MINE-1")

	require.True(t, parseResponse(t, result).Success, result)
	require.Equal(t, "15559990000@s.whatsapp.net", captured.Sender)
}

// TestSendReply_UnknownMessageDoesNotSend verifies an unsynced id fails before
// anything reaches WhatsApp, rather than sending an unquoted message.
func TestSendReply_UnknownMessageDoesNotSend(t *testing.T) {
	sendCalled := false
	client := &MockWAClient{
		SendReplyFunc: func(_ context.Context, _, _ string, _ types.QuotedContext) (string, error) {
			sendCalled = true
			return "sent-id", nil
		},
	}
	st := &MockMessageStore{
		GetMessageForDownloadFunc: func(string, *string) (store.MessageDownloadInfo, error) {
			return store.MessageDownloadInfo{}, errors.New("message not found")
		},
	}

	app := NewAppWithDeps(client, st, t.TempDir(), "test")
	result := app.SendReply(context.Background(), "someone@s.whatsapp.net", "hello", "MISSING")

	require.False(t, parseResponse(t, result).Success, "unknown quoted id must fail")
	require.False(t, sendCalled, "nothing should be sent when the quote cannot be resolved")
}

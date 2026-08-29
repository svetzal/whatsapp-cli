package client

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	goproto "google.golang.org/protobuf/proto"
)

func wrapEvent(msg *proto.Message) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:   types.NewJID("12345", types.DefaultUserServer),
				Sender: types.NewJID("67890", types.DefaultUserServer),
			},
			ID:        "MSGID",
			Timestamp: time.Unix(1700000000, 0).UTC(),
		},
		Message: msg,
	}
}

// A disappearing-message chat wraps every message. Reading the outer message
// gives a blank row, which is how a client's question went unanswered because
// it never reached the transcript.
func TestHandleMessageReadsThroughEphemeralWrapper(t *testing.T) {
	t.Parallel()

	details := HandleMessage(wrapEvent(&proto.Message{
		EphemeralMessage: &proto.FutureProofMessage{
			Message: &proto.Message{Conversation: goproto.String("any update on this?")},
		},
	}))

	assert.Equal(t, "any update on this?", details.Content)
}

func TestHandleMessageReadsThroughViewOnceWrapper(t *testing.T) {
	t.Parallel()

	details := HandleMessage(wrapEvent(&proto.Message{
		ViewOnceMessageV2: &proto.FutureProofMessage{
			Message: &proto.Message{
				ImageMessage: &proto.ImageMessage{
					Caption:  goproto.String("the error screen"),
					Mimetype: goproto.String("image/jpeg"),
				},
			},
		},
	}))

	assert.Equal(t, "the error screen", details.Content)
	if assert.NotNil(t, details.Media) {
		assert.Equal(t, "image", details.Media.Type)
	}
}

func TestHandleMessageReadsThroughNestedWrappers(t *testing.T) {
	t.Parallel()

	details := HandleMessage(wrapEvent(&proto.Message{
		EphemeralMessage: &proto.FutureProofMessage{
			Message: &proto.Message{
				DeviceSentMessage: &proto.DeviceSentMessage{
					Message: &proto.Message{Conversation: goproto.String("sent from my other phone")},
				},
			},
		},
	}))

	assert.Equal(t, "sent from my other phone", details.Content)
}

// A type we do not render should still be visible, rather than a blank line
// that looks like the contact sent nothing.
func TestHandleMessageDescribesUnrenderedTypes(t *testing.T) {
	t.Parallel()

	details := HandleMessage(wrapEvent(&proto.Message{
		StickerMessage: &proto.StickerMessage{Mimetype: goproto.String("image/webp")},
	}))

	assert.Equal(t, "[sticker]", details.Content)
}

func TestUnwrapMessageStopsOnPlainMessage(t *testing.T) {
	t.Parallel()

	plain := &proto.Message{Conversation: goproto.String("hello")}
	assert.Same(t, plain, unwrapMessage(plain))
	assert.Nil(t, unwrapMessage(nil))
}

// Package types provides shared data structures used across packages.
// This enables dependency inversion: both client and commands import types,
// rather than commands importing client.
package types

// QuotedContext identifies the message a reply quotes. WhatsApp renders the
// quote bubble from ID and Sender; Text is what a recipient sees when their
// client cannot resolve the original from its own history.
type QuotedContext struct {
	ID     string
	Sender string
	Text   string
}

// MediaDownloadRequest contains parameters for downloading media from WhatsApp.
// Used by both client (to perform download) and commands (to request download).
type MediaDownloadRequest struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileSHA256    []byte
	FileEncSHA256 []byte
	FileLength    uint64
	MediaType     string
	MimeType      string
}

// Package mail sends plain-text email over SMTP (Zoho Mail, STARTTLS on port 587).
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

// Attachment is one file attached to a Message, e.g. a calendar invite.
type Attachment struct {
	Filename    string
	ContentType string
	Content     []byte
}

type Message struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
	// Attachments is optional; when empty the message is sent as a single
	// text/plain part, unchanged from before this field existed.
	Attachments []Attachment `json:"attachments,omitempty"`
}

type Sender interface {
	Send(ctx context.Context, m Message) error
}

type SMTPSender struct {
	Addr string // host:port
	User string
	Pass string
	From string // "Name <addr@domain>"
}

func (s *SMTPSender) Send(ctx context.Context, m Message) error {
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return fmt.Errorf("parse from address: %w", err)
	}
	to, err := mail.ParseAddress(m.To)
	if err != nil {
		// The parser's error can quote the input, which is a user's address.
		return errors.New("parse to address: invalid address")
	}

	var auth smtp.Auth
	if s.User != "" {
		host, _, _ := net.SplitHostPort(s.Addr)
		auth = smtp.PlainAuth("", s.User, s.Pass, host)
	}

	msg := build(from, to, m)
	done := make(chan error, 1)
	go func() { done <- smtp.SendMail(s.Addr, auth, from.Address, []string{to.Address}, msg) }()
	select {
	case err := <-done:
		return scrubSMTPError(err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// scrubSMTPError keeps recipient addresses out of errors: servers echo the
// rejected address in their reply text (e.g. "550 5.1.1 <a@b.c>: Recipient
// address rejected"), and River and the job workers log whatever Send
// returns. Only the numeric reply code is kept for a server reply; other
// errors (dial, TLS, auth) carry host names, not addresses.
func scrubSMTPError(err error) error {
	var tpe *textproto.Error
	if errors.As(err, &tpe) {
		return fmt.Errorf("smtp reply %d", tpe.Code)
	}
	return err
}

func build(from, to *mail.Address, m Message) []byte {
	domain := from.Address[strings.LastIndexByte(from.Address, '@')+1:]
	id := make([]byte, 16)
	_, _ = rand.Read(id)

	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from.String())
	fmt.Fprintf(&b, "To: %s\r\n", to.String())
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", m.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", hex.EncodeToString(id), domain)
	b.WriteString("MIME-Version: 1.0\r\n")

	if len(m.Attachments) == 0 {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
		b.WriteString(strings.ReplaceAll(m.Text, "\n", "\r\n"))
		return b.Bytes()
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	textHeader := textproto.MIMEHeader{
		"Content-Type":              {"text/plain; charset=utf-8"},
		"Content-Transfer-Encoding": {"8bit"},
	}
	if pw, err := mw.CreatePart(textHeader); err == nil {
		_, _ = pw.Write([]byte(strings.ReplaceAll(m.Text, "\n", "\r\n")))
	}

	for _, a := range m.Attachments {
		contentType := a.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		header := textproto.MIMEHeader{
			"Content-Type":              {contentType},
			"Content-Disposition":       {fmt.Sprintf(`attachment; filename="%s"`, sanitizeFilename(a.Filename))},
			"Content-Transfer-Encoding": {"base64"},
		}
		if pw, err := mw.CreatePart(header); err == nil {
			writeBase64(pw, a.Content)
		}
	}
	mw.Close()

	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n", mw.Boundary())
	b.Write(body.Bytes())
	return b.Bytes()
}

// base64LineLen is RFC 2045 §6.8's recommended maximum encoded line length.
const base64LineLen = 76

// writeBase64 writes data to w as base64, wrapped at base64LineLen octets
// with CRLF, as required by the "base64" Content-Transfer-Encoding.
func writeBase64(w io.Writer, data []byte) {
	encoded := base64.StdEncoding.EncodeToString(data)
	for i := 0; i < len(encoded); i += base64LineLen {
		end := min(i+base64LineLen, len(encoded))
		_, _ = w.Write([]byte(encoded[i:end]))
		_, _ = w.Write([]byte("\r\n"))
	}
}

// sanitizeFilename strips CR, LF and path separators from an attachment's
// filename before it goes into a Content-Disposition header: it could in
// principle be influenced by data (an event's own content), and must never
// be able to inject extra header lines or escape the filename="..." quotes
// with a path. Mirrors the stripCRLF pattern used for other user-controlled
// header values in this codebase (internal/jobs).
func sanitizeFilename(name string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '/', '\\', '"':
			return -1
		}
		return r
	}, name)
}

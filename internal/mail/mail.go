// Package mail sends notification emails over SMTP: plain text, optionally
// with attachments.
package mail

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Security modes.
const (
	StartTLS = "starttls" // usually port 587
	TLS      = "tls"      // implicit TLS, usually port 465
	None     = "none"     // no encryption; only for a relay on a trusted network
)

type Config struct {
	Enabled  bool   `json:"enabled"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Security string `json:"security"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
	FromName string `json:"fromName"`
	// ClientCopy sends the client who submitted a copy of their answers
	// (PDF attachment). On unless an admin turns it off.
	ClientCopy bool `json:"clientCopy"`
}

// Attachment is a file sent with a message.
type Attachment struct {
	Name        string
	ContentType string
	Data        []byte
}

func (c *Config) Validate() error {
	c.Host = strings.TrimSpace(c.Host)
	c.From = strings.TrimSpace(c.From)
	c.Username = strings.TrimSpace(c.Username)
	if c.Host == "" {
		return errors.New("enter the SMTP server")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return errors.New("enter a port between 1 and 65535")
	}
	switch c.Security {
	case StartTLS, TLS, None:
	default:
		return errors.New("choose the connection security")
	}
	if _, err := mail.ParseAddress(c.From); err != nil {
		return errors.New("enter a valid sender address")
	}
	if strings.ContainsAny(c.FromName, "\r\n") {
		return errors.New("the sender name can't contain line breaks")
	}
	return nil
}

const timeout = 20 * time.Second

// Send delivers one message to all recipients (each sees only themselves
// in To), with optional attachments.
func Send(c Config, to []string, subject, body string, attachments ...Attachment) error {
	if len(to) == 0 {
		return errors.New("no recipients")
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	tlsCfg := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}

	var conn net.Conn
	var err error
	dialer := &net.Dialer{Timeout: timeout}
	if c.Security == TLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	conn.SetDeadline(time.Now().Add(2 * time.Minute))
	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("SMTP greeting: %w", err)
	}
	defer cl.Close()
	if err := cl.Hello("localhost"); err != nil {
		return err
	}
	if c.Security == StartTLS {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return errors.New("the server does not offer STARTTLS")
		}
		if err := cl.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if c.Username != "" {
		// net/smtp refuses PLAIN auth without TLS, except to localhost.
		if err := cl.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
			return fmt.Errorf("login: %w", err)
		}
	}
	from := (&mail.Address{Name: c.FromName, Address: c.From}).String()
	for _, rcpt := range to {
		if err := cl.Mail(c.From); err != nil {
			return fmt.Errorf("MAIL FROM: %w", err)
		}
		if err := cl.Rcpt(rcpt); err != nil {
			return fmt.Errorf("RCPT TO %s: %w", rcpt, err)
		}
		wc, err := cl.Data()
		if err != nil {
			return err
		}
		var msg strings.Builder
		msg.WriteString("From: " + from + "\r\n")
		msg.WriteString("To: " + rcpt + "\r\n")
		msg.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n")
		msg.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
		msg.WriteString("MIME-Version: 1.0\r\n")
		writeBody(&msg, body, attachments)
		if _, err := wc.Write([]byte(msg.String())); err != nil {
			return err
		}
		if err := wc.Close(); err != nil {
			return fmt.Errorf("sending to %s: %w", rcpt, err)
		}
		if err := cl.Reset(); err != nil {
			return err
		}
	}
	return cl.Quit()
}

func crlf(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

// writeBody writes the Content-Type headers and body: plain text alone, or
// multipart/mixed with the text first and each attachment base64-encoded.
func writeBody(msg *strings.Builder, body string, attachments []Attachment) {
	if len(attachments) == 0 {
		msg.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		msg.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
		msg.WriteString(crlf(body))
		return
	}
	boundary := "plp-" + randomBoundary()
	msg.WriteString("Content-Type: multipart/mixed; boundary=\"" + boundary + "\"\r\n\r\n")
	msg.WriteString("--" + boundary + "\r\n")
	msg.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	msg.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	msg.WriteString(crlf(body) + "\r\n")
	for _, a := range attachments {
		ct := a.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		msg.WriteString("--" + boundary + "\r\n")
		msg.WriteString("Content-Type: " + mime.FormatMediaType(ct, map[string]string{"name": a.Name}) + "\r\n")
		msg.WriteString("Content-Disposition: " + mime.FormatMediaType("attachment", map[string]string{"filename": a.Name}) + "\r\n")
		msg.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
		enc := base64.StdEncoding.EncodeToString(a.Data)
		for len(enc) > 76 {
			msg.WriteString(enc[:76] + "\r\n")
			enc = enc[76:]
		}
		msg.WriteString(enc + "\r\n")
	}
	msg.WriteString("--" + boundary + "--\r\n")
}

func randomBoundary() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

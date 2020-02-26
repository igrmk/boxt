package main

import (
	"bytes"

	"github.com/igrmk/go-smtpd/smtpd"
	"github.com/jhillyerd/enmime"
)

type env struct {
	*smtpd.BasicEnvelope
	from              smtpd.MailAddress
	data              []byte
	mime              *enmime.Envelope
	host              string
	chatIDs           map[int64]bool
	chatForUsernameCh chan<- chatForUsernameArgs
	deliverCh         chan<- deliverArgs
	maxSize           int
}

type chatForUsernameArgs struct {
	result   chan *int64
	username string
}

type deliverArgs struct {
	result chan error
	env    *env
}

// Close implements smtpd.Envelope.Close
func (e *env) Close() error {
	if len(e.chatIDs) == 0 {
		return smtpd.SMTPError("550 bad recipient")
	}
	mime, err := enmime.ReadEnvelope(bytes.NewReader(e.data))
	if err != nil {
		return err
	}
	e.mime = mime
	if err := e.deliver(); err != nil {
		return err
	}
	return nil
}

// Write implements smtpd.Envelope.Write
func (e *env) Write(line []byte) error {
	e.data = append(e.data, line...)
	if len(e.data) > e.maxSize {
		return smtpd.SMTPError("552 5.3.4 message too big")
	}
	return nil
}

// AddRecipient implements smtpd.Envelope.AddRecipient
func (e *env) AddRecipient(rcpt smtpd.MailAddress) error {
	username, host := splitAddress(rcpt.Email())
	if host != e.host {
		return smtpd.SMTPError("550 bad recipient")
	}
	chatID := e.chatForUsername(username)
	if chatID == nil {
		return smtpd.SMTPError("550 bad recipient")
	}
	e.chatIDs[*chatID] = true
	return e.BasicEnvelope.AddRecipient(rcpt)
}

func (e *env) deliver() error {
	result := make(chan error)
	defer close(result)
	e.deliverCh <- deliverArgs{result: result, env: e}
	return <-result
}

func (e *env) chatForUsername(username string) *int64 {
	result := make(chan *int64)
	defer close(result)
	e.chatForUsernameCh <- chatForUsernameArgs{result: result, username: username}
	return <-result
}

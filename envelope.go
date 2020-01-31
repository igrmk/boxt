package main

import (
	"bytes"

	"github.com/bradfitz/go-smtpd/smtpd"
	"github.com/jhillyerd/enmime"
)

type env struct {
	*smtpd.BasicEnvelope
	from            smtpd.MailAddress
	data            []byte
	mime            *enmime.Envelope
	chatIDs         map[int64][]string
	chatForUsername chan<- chatForUsernameArgs
	deliver         chan<- *env
	delivered       chan bool
	host            string
}

type chatForUsernameArgs struct {
	chatForUsername chan *int64
	username        string
}

// Close implements smtpd.Envelope.Close
func (e *env) Close() error {
	mime, err := enmime.ReadEnvelope(bytes.NewReader(e.data))
	if err != nil {
		return err
	}
	e.mime = mime
	e.delivered = make(chan bool)
	defer close(e.delivered)
	e.deliver <- e
	delivered := <-e.delivered
	if !delivered {
		return smtpd.SMTPError("450 mailbox unavailable")
	}
	return nil
}

// Write implements smtpd.Envelope.Write
func (e *env) Write(line []byte) error {
	e.data = append(e.data, line...)
	return nil
}

// AddRecipient implements smtpd.Envelope.AddRecipient
func (e *env) AddRecipient(rcpt smtpd.MailAddress) error {
	username, host := splitAddress(rcpt.Email())
	if host != e.host {
		return smtpd.SMTPError("550 bad recipient")
	}
	chatForUsername := make(chan *int64)
	defer close(chatForUsername)
	e.chatForUsername <- chatForUsernameArgs{chatForUsername: chatForUsername, username: username}
	chatID := <-chatForUsername
	if chatID == nil {
		return smtpd.SMTPError("550 bad recipient")
	}
	e.chatIDs[*chatID] = append(e.chatIDs[*chatID], username)
	return e.BasicEnvelope.AddRecipient(rcpt)
}

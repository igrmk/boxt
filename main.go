package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	tg "github.com/bcmk/telegram-bot-api"
	"github.com/bradfitz/go-smtpd/smtpd"
	"github.com/jhillyerd/enmime"
	_ "github.com/mattn/go-sqlite3"
)

// checkErr panics on an error
func checkErr(err error) {
	if err != nil {
		panic(err)
	}
}

type worker struct {
	bot    *tg.BotAPI
	db     *sql.DB
	cfg    *config
	client *http.Client
}

func newWorker() *worker {
	if len(os.Args) != 2 {
		panic("usage: boxt <config>")
	}
	cfg := readConfig(os.Args[1])
	client := HTTPClientWithTimeoutAndAddress(cfg.TimeoutSeconds)
	bot, err := tg.NewBotAPIWithClient(cfg.BotToken, client)
	checkErr(err)
	db, err := sql.Open("sqlite3", cfg.DBPath)
	checkErr(err)
	w := &worker{
		bot:    bot,
		db:     db,
		cfg:    cfg,
		client: client,
	}

	return w
}

type env struct {
	*smtpd.BasicEnvelope
	from  smtpd.MailAddress
	data  []byte
	mime  *enmime.Envelope
	rcpts []smtpd.MailAddress
	ch    chan<- *env
}

type address struct {
	chatID   int64
	username string
	muted    bool
}

func (e *env) Close() error {
	mime, err := enmime.ReadEnvelope(bytes.NewReader(e.data))
	if err != nil {
		return err
	}
	e.mime = mime
	e.ch <- e
	return nil
}

func (e *env) Write(line []byte) error {
	e.data = append(e.data, line...)
	return nil
}

func splitAddress(a string) (string, string) {
	a = strings.ToLower(a)
	parts := strings.Split(a, "@")
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func (w *worker) received(e *env) {
	chatIDs := make(map[int64]bool)
	for _, r := range e.rcpts {
		username, host := splitAddress(r.Email())
		if host != w.cfg.Host {
			continue
		}
		address := w.addressForUsername(username)
		if address != nil && !address.muted {
			chatIDs[address.chatID] = true
		}
	}

	for chatID := range chatIDs {
		text := fmt.Sprintf(
			"Subject: %s\nFrom: %s\nTo: %s\n\n%s",
			e.mime.GetHeader("Subject"),
			e.mime.GetHeader("From"),
			e.mime.GetHeader("To"), e.mime.Text)
		w.send(chatID, true, parseRaw, text)
		for _, inline := range e.mime.Inlines {
			b := tg.FileBytes{Name: inline.FileName, Bytes: inline.Content}
			switch {
			case strings.HasPrefix(inline.ContentType, "image/"):
				msg := tg.NewPhotoUpload(chatID, b)
				w.bot.Send(msg)
			default:
				msg := tg.NewDocumentUpload(chatID, b)
				w.bot.Send(msg)
			}
		}
		for _, inline := range e.mime.Attachments {
			b := tg.FileBytes{Name: inline.FileName, Bytes: inline.Content}
			msg := tg.NewDocumentUpload(chatID, b)
			w.bot.Send(msg)
		}
	}
}

func (e *env) AddRecipient(rcpt smtpd.MailAddress) error {
	e.rcpts = append(e.rcpts, rcpt)
	return e.BasicEnvelope.AddRecipient(rcpt)
}

func mailFactory(ch chan *env) func(smtpd.Connection, smtpd.MailAddress) (smtpd.Envelope, error) {
	return func(c smtpd.Connection, from smtpd.MailAddress) (smtpd.Envelope, error) {
		return &env{BasicEnvelope: &smtpd.BasicEnvelope{}, from: from, ch: ch}, nil
	}
}

func (w *worker) logConfig() {
	cfgString, err := json.MarshalIndent(w.cfg, "", "    ")
	checkErr(err)
	linf("config: " + string(cfgString))
}

func (w *worker) setWebhook() {
	linf("setting webhook...")
	var _, err = w.bot.SetWebhook(tg.NewWebhook(path.Join(w.cfg.Host, w.cfg.ListenPath)))
	checkErr(err)
	info, err := w.bot.GetWebhookInfo()
	checkErr(err)
	if info.LastErrorDate != 0 {
		linf("last webhook error time: %v", time.Unix(int64(info.LastErrorDate), 0))
	}
	if info.LastErrorMessage != "" {
		linf("last webhook error message: %s", info.LastErrorMessage)
	}
	linf("OK")
}

func (w *worker) removeWebhook() {
	linf("removing webhook...")
	_, err := w.bot.RemoveWebhook()
	checkErr(err)
	linf("OK")
}

func (w *worker) mustExec(query string, args ...interface{}) {
	stmt, err := w.db.Prepare(query)
	checkErr(err)
	_, err = stmt.Exec(args...)
	checkErr(err)
	stmt.Close()
}

func (w *worker) createDatabase() {
	linf("creating database if needed...")
	w.mustExec(`
		create table if not exists feedback (
			chat_id integer,
			text text);`)
	w.mustExec(`
		create table if not exists users (
			chat_id integer primary key);`)
	w.mustExec(`
		create table if not exists addresses (
			chat_id integer,
			username text not null default '',
			muted integer not null default 0);`)
}

const letterBytes = "abcdefghijklmnopqrstuvwxyz"

func randString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	return string(b)
}

func (w *worker) start(chatID int64) {
	exists := w.db.QueryRow("select count(*) from users where chat_id=?", chatID)
	if singleInt(exists) == 0 {
		w.mustExec("insert into users (chat_id) values (?)", chatID)
		for i := 0; i < w.cfg.FreeEmails; i++ {
			username := ""
			for {
				username = randString(5)
				exists := w.db.QueryRow("select count(*) from addresses where username=?", username)
				if singleInt(exists) == 0 {
					break
				}
			}
			w.mustExec("insert into addresses (chat_id, username) values (?,?)", chatID, username)
		}
	}
	lines := w.addressStrings(w.addressesOfUser(chatID))
	lines = append([]string{"We created 10 email addreses for you. An email sent to any of these addresses will appear here in the chat"}, lines...)
	w.send(chatID, false, parseRaw, strings.Join(lines, "\n"))
}

func (w *worker) processIncomingCommand(chatID int64, command, arguments string) {
	command = strings.ToLower(command)
	linf("chat: %d, command: %s %s", chatID, command, arguments)

	switch command {
	case "addresses":
		w.listAddresses(chatID)
	case "feedback":
		w.feedback(chatID, arguments)
	case "start":
		w.start(chatID)
	case "mute":
		w.mute(chatID, arguments)
	case "unmute":
		w.unmute(chatID, arguments)
	default:
		w.send(chatID, false, parseRaw, "Unknown command")
	}
}

func (w *worker) processTGUpdate(u tg.Update) {
	if u.Message != nil && u.Message.Chat != nil {
		if u.Message.IsCommand() {
			w.processIncomingCommand(u.Message.Chat.ID, u.Message.Command(), u.Message.CommandArguments())
		} else {
			parts := strings.SplitN(u.Message.Text, " ", 2)
			for len(parts) < 2 {
				parts = append(parts, "")
			}
			w.processIncomingCommand(u.Message.Chat.ID, parts[0], parts[1])
		}
	}
}

func (w *worker) feedback(chatID int64, text string) {
	if text == "" {
		w.send(chatID, false, parseRaw, "Command format: /feedback <text>")
		return
	}
	w.mustExec("insert into feedback (chat_id, text) values (?, ?, ?)", chatID, text)
	w.send(chatID, false, parseRaw, "Thank you for your feedback")
	w.send(w.cfg.AdminID, true, parseRaw, fmt.Sprintf("Feedback: %s", text))
}

func (w *worker) mute(chatID int64, address string) {
	if address == "" {
		w.send(chatID, false, parseRaw, "Command format: /mute <email@boxt.us>")
		return
	}
	username, host := splitAddress(address)
	if host != w.cfg.Host {
		w.send(chatID, false, parseRaw, "Address not found")
		return
	}
	exists := w.db.QueryRow("select count(*) from addresses where chat_id=? and username=?", chatID, username)
	if singleInt(exists) == 0 {
		w.send(chatID, false, parseRaw, "Address not found")
		return
	}
	w.mustExec("update addresses set muted=1 where chat_id=? and username=?", chatID, username)
	w.send(chatID, false, parseRaw, "OK")
}

func (w *worker) unmute(chatID int64, address string) {
	if address == "" {
		w.send(chatID, false, parseRaw, "Command format: /unmute <email@boxt.us>")
		return
	}
	username, host := splitAddress(address)
	if host != w.cfg.Host {
		w.send(chatID, false, parseRaw, "Address not found")
		return
	}
	exists := w.db.QueryRow("select count(*) from addresses where chat_id=? and username=?", chatID, username)
	if singleInt(exists) == 0 {
		w.send(chatID, false, parseRaw, "Address not found")
		return
	}
	w.mustExec("update addresses set muted=0 where chat_id=? and username=?", chatID, username)
	w.send(chatID, false, parseRaw, "OK")
}

func (w *worker) send(chatID int64, notify bool, parse parseKind, text string) {
	msg := tg.NewMessage(chatID, text)
	msg.DisableNotification = !notify
	switch parse {
	case parseHTML, parseMarkdown:
		msg.ParseMode = parse.String()
	}
	_, err := w.bot.Send(msg)

	if err != nil {
		switch err := err.(type) {
		case tg.Error:
			lerr("cannot send a message to %d, %v", chatID, err)
		default:
			lerr("unexpected error type while sending a message to %d, %v", chatID, err)
		}
	}
}

func (w *worker) addressStrings(addresses []address) []string {
	result := make([]string, len(addresses))
	for i, l := range addresses {
		result[i] = l.username + "@" + w.cfg.Host
	}
	return result
}

func (w *worker) listAddresses(chatID int64) {
	addrs := w.addressesOfUser(chatID)
	active := []address{}
	muted := []address{}
	for _, a := range addrs {
		if a.muted {
			muted = append(muted, a)
		} else {
			active = append(active, a)
		}
	}
	lines := []string{}
	if len(active) > 0 {
		lines = append(lines, "ACTIVE")
		lines = append(lines, w.addressStrings(active)...)
	}
	if len(muted) > 0 {
		if len(active) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "MUTED")
		lines = append(lines, w.addressStrings(muted)...)
	}
	w.send(chatID, false, parseRaw, strings.Join(lines, "\n"))
}

func (w *worker) addressForUsername(username string) *address {
	modelsQuery, err := w.db.Query(`
		select chat_id, muted from addresses
		where username=?`,
		username)
	checkErr(err)
	defer modelsQuery.Close()
	if modelsQuery.Next() {
		address := address{username: username}
		checkErr(modelsQuery.Scan(&address.chatID, &address.muted))
		return &address
	}
	return nil
}

func (w *worker) addressesOfUser(chatID int64) (addresses []address) {
	modelsQuery, err := w.db.Query(`
		select username, muted from addresses
		where chat_id=?
		order by username`,
		chatID)
	checkErr(err)
	defer modelsQuery.Close()
	for modelsQuery.Next() {
		address := address{chatID: chatID}
		checkErr(modelsQuery.Scan(&address.username, &address.muted))
		addresses = append(addresses, address)
	}
	return
}

func main() {
	w := newWorker()
	w.logConfig()
	w.setWebhook()
	w.createDatabase()
	incoming := w.bot.ListenForWebhook(w.cfg.Host + w.cfg.ListenPath)

	mail := make(chan *env)
	s := &smtpd.Server{
		Hostname:  w.cfg.Host,
		Addr:      w.cfg.MailAddress,
		OnNewMail: mailFactory(mail),
	}
	go func() {
		err := s.ListenAndServe()
		if err != nil {
			log.Fatalf("ListenAndServe (email): %v", err)
		}
	}()
	go func() {
		err := http.ListenAndServe(w.cfg.ListenAddress, nil)
		if err != nil {
			log.Fatalf("ListenAndServe (telegram): %v", err)
		}
	}()
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGABRT, syscall.SIGKILL)
	for {
		select {
		case m := <-mail:
			w.received(m)
		case m := <-incoming:
			w.processTGUpdate(m)
		case s := <-signals:
			linf("got signal %v", s)
			w.removeWebhook()
			return
		}
	}
}

package main

import (
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	tg "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/igrmk/go-smtpd/smtpd"
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
	tls    *tls.Config
}

func newWorker() *worker {
	if len(os.Args) != 2 {
		panic("usage: boxt <config>")
	}
	cfg := readConfig(os.Args[1])
	tls, err := loadTLS(cfg.Certificate, cfg.CertificateKey)
	checkErr(err)
	client := &http.Client{Timeout: time.Second * time.Duration(cfg.TimeoutSeconds)}
	bot, err := tg.NewBotAPIWithClient(cfg.BotToken, tg.APIEndpoint, client)
	checkErr(err)
	db, err := sql.Open("sqlite3", cfg.DBPath)
	checkErr(err)
	w := &worker{
		bot:    bot,
		db:     db,
		cfg:    cfg,
		client: client,
		tls:    tls,
	}

	return w
}

type address struct {
	chatID       int64
	username     string
	muted        bool
	nextDelivery int64
}

var errorMuted = errors.New("Mailbox is muted")
var errorTooManyEmails = errors.New("Too many emails")

func splitAddress(a string) (string, string) {
	a = strings.ToLower(a)
	parts := strings.Split(a, "@")
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func (w *worker) deliver(e *env) error {
	messageID := e.mime.GetHeader("Message-ID")
	if messageID == "" {
		return smtpd.SMTPError("501 Message-ID is not specified")
	}

	subject := e.mime.GetHeader("Subject")
	from := e.mime.GetHeader("From")
	to := e.mime.GetHeader("To")

	if !utf8.ValidString(subject) {
		return smtpd.SMTPError("501 Subject is invalid")
	}
	if !utf8.ValidString(from) {
		return smtpd.SMTPError("501 From is invalid")
	}
	if !utf8.ValidString(to) {
		return smtpd.SMTPError("501 To is invalid")
	}
	if !utf8.ValidString(e.mime.Text) {
		return smtpd.SMTPError("501 message is invalid")
	}

	text := fmt.Sprintf("Subject: %s\nFrom: %s\nTo: %s\n\n%s", subject, from, to, e.mime.Text)

	delivered := true
	for chatID := range e.chatIDs {
		duplicates := w.db.QueryRow("select count(*) from delivered_ids where chat_id=? and message_id=?", chatID, messageID)
		if singleInt(duplicates) == 0 {
			delivered = w.deliverToChat(chatID, messageID, text, e) && delivered
		}
	}
	if !delivered {
		return smtpd.SMTPError("450 mailbox unavailable")
	}
	return nil
}

func chunks(s string, chunkSize int) (chunks []string) {
	if len(s) == 0 {
		return nil
	}
	runes := []rune(s)
	i := 0
	for i < len(runes)-chunkSize {
		eol := i + chunkSize
		for ; eol > i; eol-- {
			if runes[eol] == '\n' {
				break
			}
		}
		if eol == i {
			chunks = append(chunks, string(runes[i:i+chunkSize]))
			i += chunkSize
		} else {
			chunks = append(chunks, string(runes[i:eol]))
			i = eol + 1
		}
	}
	chunks = append(chunks, string(runes[i:]))
	return
}

func (w *worker) deliverToChat(chatID int64, messageID string, text string, e *env) bool {
	chunks := chunks(text, w.cfg.MaxTextChunkSize)
	for _, c := range chunks {
		if w.sendText(chatID, true, parseRaw, c) != nil {
			return false
		}
	}
	for _, inline := range e.mime.Inlines {
		b := tg.FileBytes{Name: inline.FileName, Bytes: inline.Content}
		switch {
		case strings.HasPrefix(inline.ContentType, "image/"):
			msg := tg.NewPhoto(chatID, b)
			if w.send(&photoConfig{msg}) != nil {
				return false
			}
		case strings.HasPrefix(inline.ContentType, "video/"):
			msg := tg.NewVideo(chatID, b)
			if w.send(&videoConfig{msg}) != nil {
				return false
			}
		case strings.HasPrefix(inline.ContentType, "audio/"):
			msg := tg.NewAudio(chatID, b)
			if w.send(&audioConfig{msg}) != nil {
				return false
			}
		default:
			msg := tg.NewDocument(chatID, b)
			if w.send(&documentConfig{msg}) != nil {
				return false
			}
		}
	}
	for _, inline := range e.mime.Attachments {
		b := tg.FileBytes{Name: inline.FileName, Bytes: inline.Content}
		msg := tg.NewDocument(chatID, b)
		if w.send(&documentConfig{msg}) != nil {
			return false
		}
	}
	w.mustExec("insert into delivered_ids (chat_id, message_id) values (?,?)", chatID, messageID)
	return true
}

func (w *worker) chatForUsername(u chatForUsernameArgs) (int64, error) {
	address := w.addressForUsername(u.username)
	if address == nil || address.muted {
		return 0, errorMuted
	}
	now := time.Now().Unix()
	if address.nextDelivery > now {
		return 0, errorTooManyEmails
	}
	address.nextDelivery += int64(w.cfg.LimitIntervalSeconds)
	if now-int64(w.cfg.LimitWindowSeconds) > address.nextDelivery {
		address.nextDelivery = now - int64(w.cfg.LimitWindowSeconds)
	}
	w.mustExec("update addresses set next_delivery=? where username=?", address.nextDelivery, u.username)
	return address.chatID, nil
}

func envelopeFactory(deliverCh chan deliverArgs, chatForUsernameCh chan chatForUsernameArgs, host string, maxSize int) func(smtpd.Connection, smtpd.MailAddress, *int) (smtpd.Envelope, error) {
	return func(c smtpd.Connection, from smtpd.MailAddress, size *int) (smtpd.Envelope, error) {
		if size != nil && *size > maxSize {
			return nil, smtpd.SMTPError("552 5.3.4 message too big")
		}
		return &env{
			BasicEnvelope:     &smtpd.BasicEnvelope{},
			from:              from,
			deliverCh:         deliverCh,
			chatForUsernameCh: chatForUsernameCh,
			chatIDs:           make(map[int64]bool),
			host:              host,
			maxSize:           maxSize,
		}, nil
	}
}

func (w *worker) logConfig() {
	cfgString, err := json.MarshalIndent(w.cfg, "", "    ")
	checkErr(err)
	linf("config: " + string(cfgString))
}

func (w *worker) setWebhook() {
	linf("setting webhook...")
	wh, err := tg.NewWebhook(path.Join(w.cfg.Host, w.cfg.ListenPath))
	checkErr(err)
	_, err = w.bot.Request(wh)
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
	_, err := w.bot.Request(tg.DeleteWebhookConfig{})
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

const letterBytes = "abcdefghijklmnopqrstuvwxyz"

func randString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	return string(b)
}

func (w *worker) newRandUsername() (username string) {
	for {
		username = randString(5)
		exists := w.db.QueryRow("select count(*) from addresses where username=?", username)
		if singleInt(exists) == 0 {
			break
		}
	}
	return
}

func (w *worker) newRandExternalID() (id string) {
	for {
		id = randString(5)
		exists := w.db.QueryRow("select count(*) from users where external_id=?", id)
		if singleInt(exists) == 0 {
			break
		}
	}
	return
}

func (w *worker) refer(referrer string) bool {
	chatID := w.chatForExternalID(referrer)
	if chatID == nil {
		return false
	}
	for i := 0; i < w.cfg.ReferralBonus; i++ {
		username := w.newRandExternalID()
		w.mustExec("insert into addresses (chat_id, username) values (?,?)", *chatID, username)
	}
	return true
}

func (w *worker) externalID(chatID int64) *string {
	query, err := w.db.Query("select external_id from users where chat_id=?", chatID)
	checkErr(err)
	defer query.Close()
	if !query.Next() {
		return nil
	}
	var externalID string
	checkErr(query.Scan(&externalID))
	return &externalID
}

func (w *worker) chatForExternalID(externalID string) *int64 {
	query, err := w.db.Query("select chat_id from users where external_id=?", externalID)
	checkErr(err)
	defer query.Close()
	if !query.Next() {
		return nil
	}
	var chatID int64
	checkErr(query.Scan(&chatID))
	return &chatID
}

func (w *worker) start(chatID int64, referrer string) {
	w.mustExec("update addresses set next_delivery=0 where chat_id=?", chatID)
	externalID := w.externalID(chatID)
	if externalID == nil {
		referOK := false
		if chatID > 0 && referrer != "" {
			referOK = w.refer(referrer)
		}
		temp := w.newRandUsername()
		externalID = &temp
		w.mustExec("insert into users (chat_id, external_id) values (?, ?)", chatID, *externalID)
		emails := w.cfg.FreeEmails
		if referOK {
			emails += w.cfg.FollowerBonus
		}
		for i := 0; i < emails; i++ {
			username := w.newRandUsername()
			w.mustExec("insert into addresses (chat_id, username) values (?,?)", chatID, username)
		}
	}
	if *externalID == referrer {
		_ = w.sendText(chatID, false, parseRaw, "You've just hit your own referral link")
		return
	}
	usernames := w.usernamesForChat(chatID)
	lines := w.addressStrings(usernames)
	referralLink := fmt.Sprintf("https://t.me/%s?start=%s", w.cfg.BotName, *externalID)
	welcome := fmt.Sprintf("We created %d email addreses for you. An email sent to any of these addresses will appear here in the chat", len(usernames))
	lines = append([]string{welcome}, lines...)
	referralLine := fmt.Sprintf("\nEarn addresses by sharing the referral link!\n%s\n\nYou will get %d addresses for every new registered user\nNew user will get %d additional addresses",
		referralLink,
		w.cfg.ReferralBonus,
		w.cfg.FollowerBonus)
	lines = append(lines, referralLine)
	_ = w.sendText(chatID, false, parseRaw, strings.Join(lines, "\n"))
}

func (w *worker) broadcastChats() (chats []int64) {
	chatsQuery, err := w.db.Query(`select chat_id from users`)
	checkErr(err)
	defer chatsQuery.Close()
	for chatsQuery.Next() {
		var chatID int64
		checkErr(chatsQuery.Scan(&chatID))
		chats = append(chats, chatID)
	}
	return
}

func (w *worker) broadcast(text string) {
	if text == "" {
		return
	}
	if w.cfg.Debug {
		ldbg("broadcasting")
	}
	chats := w.broadcastChats()
	for _, chatID := range chats {
		_ = w.sendText(chatID, true, parseRaw, text)
	}
	_ = w.sendText(w.cfg.AdminID, false, parseRaw, "OK")
}

func (w *worker) direct(arguments string) {
	parts := strings.SplitN(arguments, " ", 2)
	if len(parts) < 2 {
		_ = w.sendText(w.cfg.AdminID, false, parseRaw, "Usage: /direct chatID text")
		return
	}
	whom, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		_ = w.sendText(w.cfg.AdminID, false, parseRaw, "First argument is invalid")
		return
	}
	text := parts[1]
	if text == "" {
		return
	}
	_ = w.sendText(whom, true, parseRaw, text)
	_ = w.sendText(w.cfg.AdminID, false, parseRaw, "OK")
}

func (w *worker) addUsername(arguments string) {
	parts := strings.SplitN(arguments, " ", 2)
	if len(parts) < 2 {
		_ = w.sendText(w.cfg.AdminID, false, parseRaw, "Usage: /add_username chatID email")
		return
	}
	chatID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		_ = w.sendText(w.cfg.AdminID, false, parseRaw, "First argument is invalid")
		return
	}
	username := parts[1]
	if username == "" {
		return
	}
	w.mustExec("insert into addresses (chat_id, username) values (?,?)", chatID, username)
	_ = w.sendText(w.cfg.AdminID, false, parseRaw, "OK")
}

func (w *worker) removeUser(arguments string) {
	chatID, err := strconv.ParseInt(arguments, 10, 64)
	if err != nil {
		_ = w.sendText(w.cfg.AdminID, false, parseRaw, "Argument is invalid")
		return
	}
	w.mustExec("delete from addresses where chat_id=?", chatID)
	w.mustExec("delete from users where chat_id=?", chatID)
	_ = w.sendText(w.cfg.AdminID, false, parseRaw, "OK")
}

func (w *worker) processAdminMessage(chatID int64, command, arguments string) bool {
	switch command {
	case "stat":
		w.stat()
		return true
	case "broadcast":
		w.broadcast(arguments)
		return true
	case "direct":
		w.direct(arguments)
		return true
	case "add_username":
		w.addUsername(arguments)
		return true
	case "remove_user":
		w.removeUser(arguments)
		return true
	}
	return false
}

func (w *worker) processIncomingCommand(chatID int64, command, arguments string) {
	command = strings.ToLower(command)
	linf("chat: %d, command: %s %s", chatID, command, arguments)
	if chatID == w.cfg.AdminID && w.processAdminMessage(chatID, command, arguments) {
		return
	}
	switch command {
	case "addresses":
		w.listAddresses(chatID)
	case "feedback":
		w.feedback(chatID, arguments)
	case "start":
		w.start(chatID, arguments)
	case "mute":
		w.mute(chatID, arguments)
	case "unmute":
		w.unmute(chatID, arguments)
	case "referral":
		w.referralLink(chatID)
	case "source":
		_ = w.sendText(chatID, false, parseRaw, "Source code: https://github.com/igrmk/boxt")
	default:
		_ = w.sendText(chatID, false, parseRaw, "Unknown command")
	}
}

func (w *worker) ourID() int64 {
	if idx := strings.Index(w.cfg.BotToken, ":"); idx != -1 {
		id, err := strconv.ParseInt(w.cfg.BotToken[:idx], 10, 64)
		checkErr(err)
		return id
	}
	checkErr(errors.New("cannot get our ID"))
	return 0
}

func (w *worker) processTGUpdate(u tg.Update) {
	if u.Message != nil && u.Message.Chat != nil {
		if newMembers := u.Message.NewChatMembers; len(newMembers) > 0 {
			ourID := w.ourID()
			for _, m := range newMembers {
				if int64(m.ID) == ourID {
					w.start(u.Message.Chat.ID, "")
					break
				}
			}
		} else if u.Message.IsCommand() {
			w.processIncomingCommand(u.Message.Chat.ID, u.Message.Command(), u.Message.CommandArguments())
		} else {
			if u.Message.Text == "" {
				return
			}
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
		_ = w.sendText(chatID, false, parseRaw, "Command format: /feedback <text>")
		return
	}
	w.mustExec("insert into feedback (chat_id, text) values (?, ?)", chatID, text)
	_ = w.sendText(chatID, false, parseRaw, "Thank you for your feedback")
	_ = w.sendText(w.cfg.AdminID, true, parseRaw, fmt.Sprintf("Feedback: %s", text))
}

func (w *worker) mute(chatID int64, address string) {
	if address == "" {
		_ = w.sendText(chatID, false, parseRaw, "Command format: /mute <email@boxt.us>")
		return
	}
	username, host := splitAddress(address)
	if host != w.cfg.Host {
		_ = w.sendText(chatID, false, parseRaw, "Address not found")
		return
	}
	exists := w.db.QueryRow("select count(*) from addresses where chat_id=? and username=?", chatID, username)
	if singleInt(exists) == 0 {
		_ = w.sendText(chatID, false, parseRaw, "Address not found")
		return
	}
	w.mustExec("update addresses set muted=1 where username=?", username)
	_ = w.sendText(chatID, false, parseRaw, "OK")
}

func (w *worker) unmute(chatID int64, address string) {
	if address == "" {
		_ = w.sendText(chatID, false, parseRaw, "Command format: /unmute <email@boxt.us>")
		return
	}
	username, host := splitAddress(address)
	if host != w.cfg.Host {
		_ = w.sendText(chatID, false, parseRaw, "Address not found")
		return
	}
	exists := w.db.QueryRow("select count(*) from addresses where chat_id=? and username=?", chatID, username)
	if singleInt(exists) == 0 {
		_ = w.sendText(chatID, false, parseRaw, "Address not found")
		return
	}
	w.mustExec("update addresses set muted=0 where username=?", username)
	_ = w.sendText(chatID, false, parseRaw, "OK")
}

func (w *worker) referralLink(chatID int64) {
	if chatID < 0 {
		_ = w.sendText(chatID, false, parseRaw, "Referral links don't work for group chats")
		return
	}
	externalID := w.externalID(chatID)
	if externalID == nil {
		return
	}
	referralLink := fmt.Sprintf("https://t.me/%s?start=%s", w.cfg.BotName, *externalID)
	referralLine := fmt.Sprintf("Your referral link is %s\nYou will get %d addresses for every new registered user\nNew user will get %d additional addresses",
		referralLink,
		w.cfg.ReferralBonus,
		w.cfg.FollowerBonus)
	_ = w.sendText(chatID, false, parseRaw, referralLine)
}

func (w *worker) userCount() int {
	query := w.db.QueryRow("select count(*) from users")
	return singleInt(query)
}

func (w *worker) activeUserCount() int {
	query := w.db.QueryRow("select count(distinct chat_id) from delivered_ids")
	return singleInt(query)
}

func (w *worker) emailCount() int {
	query := w.db.QueryRow("select count(*) from delivered_ids")
	return singleInt(query)
}

func (w *worker) addressCount() int {
	query := w.db.QueryRow("select count(*) from addresses")
	return singleInt(query)
}

func (w *worker) activeAddressCount() int {
	query := w.db.QueryRow("select count(*) from addresses where muted=0")
	return singleInt(query)
}

func (w *worker) stat() {
	lines := []string{}
	lines = append(lines, fmt.Sprintf("users: %d", w.userCount()))
	lines = append(lines, fmt.Sprintf("active users: %d", w.activeUserCount()))
	lines = append(lines, fmt.Sprintf("emails: %d", w.emailCount()))
	lines = append(lines, fmt.Sprintf("addresses: %d/%d", w.activeAddressCount(), w.addressCount()))
	_ = w.sendText(w.cfg.AdminID, false, parseRaw, strings.Join(lines, "\n"))
}

func (w *worker) sendText(chatID int64, notify bool, parse parseKind, text string) error {
	msg := tg.NewMessage(chatID, text)
	msg.DisableNotification = !notify
	switch parse {
	case parseHTML, parseMarkdown:
		msg.ParseMode = parse.String()
	}
	return w.send(&messageConfig{msg})
}

func (w *worker) send(msg baseChattable) error {
	if _, err := w.bot.Send(msg); err != nil {
		switch err := err.(type) {
		case *tg.Error:
			lerr("cannot send a message to %d, %v", msg.baseChat().ChatID, err)
			if err.Code == 403 {
				nextDelivery := time.Now().Unix() + int64(w.cfg.BlockedBackoffSeconds)
				w.mustExec(
					"update addresses set next_delivery=? where next_delivery<? and chat_id=?",
					nextDelivery,
					nextDelivery,
					msg.baseChat().ChatID)
			}
		default:
			lerr("unexpected error type while sending a message to %d, %v", msg.baseChat().ChatID, err)
		}
		return err
	}
	return nil
}

func (w *worker) addressStrings(addresses []address) []string {
	result := make([]string, len(addresses))
	for i, l := range addresses {
		result[i] = l.username + "@" + w.cfg.Host
	}
	return result
}

func (w *worker) listAddresses(chatID int64) {
	addrs := w.usernamesForChat(chatID)
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
	externalID := w.externalID(chatID)
	if externalID == nil {
		return
	}
	referralLink := fmt.Sprintf("https://t.me/%s?start=%s", w.cfg.BotName, *externalID)
	referralLine := fmt.Sprintf("\nEarn addresses by sharing the referral link!\n%s\n\nYou will get %d addresses for every new registered user\nNew user will get %d additional addresses",
		referralLink,
		w.cfg.ReferralBonus,
		w.cfg.FollowerBonus)
	lines = append(lines, referralLine)
	_ = w.sendText(chatID, false, parseRaw, strings.Join(lines, "\n"))
}

func (w *worker) addressForUsername(username string) *address {
	modelsQuery, err := w.db.Query(`
		select chat_id, muted, next_delivery from addresses
		where username=?`,
		username)
	checkErr(err)
	defer modelsQuery.Close()
	if modelsQuery.Next() {
		address := address{username: username}
		checkErr(modelsQuery.Scan(&address.chatID, &address.muted, &address.nextDelivery))
		return &address
	}
	return nil
}

func (w *worker) usernamesForChat(chatID int64) (usernames []address) {
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
		usernames = append(usernames, address)
	}
	return
}

func loadTLS(certFile string, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

func main() {
	rand.Seed(time.Now().UnixNano())
	w := newWorker()
	w.logConfig()
	w.setWebhook()
	w.createDatabase()
	incoming := w.bot.ListenForWebhook(w.cfg.Host + w.cfg.ListenPath)

	deliverCh := make(chan deliverArgs)
	chatForUsernameCh := make(chan chatForUsernameArgs)
	smtp := &smtpd.Server{
		Hostname:  w.cfg.Host,
		Addr:      w.cfg.MailAddress,
		OnNewMail: envelopeFactory(deliverCh, chatForUsernameCh, w.cfg.Host, w.cfg.MaxSize),
		TLSConfig: w.tls,
		MaxSize:   w.cfg.MaxSize,
		Log:       lsmtpd,
	}
	go func() {
		err := smtp.ListenAndServe()
		checkErr(err)
	}()
	go func() {
		err := http.ListenAndServe(w.cfg.ListenAddress, nil)
		checkErr(err)
	}()
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGABRT)
	for {
		select {
		case m := <-deliverCh:
			err := w.deliver(m.env)
			if err != nil {
				linf("delivery failed: %v", err)
			}
			m.result <- err
		case u := <-chatForUsernameCh:
			chatID, err := w.chatForUsername(u)
			u.result <- chatForUsernameResult{chatID: chatID, err: err}
		case m := <-incoming:
			w.processTGUpdate(m)
		case s := <-signals:
			linf("got signal %v", s)
			w.removeWebhook()
			return
		}
	}
}

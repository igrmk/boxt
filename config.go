package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

type config struct {
	MailAddress    string `json:"mail_address"`    // the address to listen to incoming mail
	MaxSize        int    `json:"max_size"`        // the maximum email size in bytes
	ListenPath     string `json:"listen_path"`     // the path excluding domain to listen to, the good choice is "/your-telegram-bot-token"
	ListenAddress  string `json:"listen_address"`  // the address to listen to incoming telegram messages
	Host           string `json:"host"`            // the host name for the email addresses and the webhook
	BotToken       string `json:"bot_token"`       // your telegram bot token
	FreeEmails     int    `json:"free_emails"`     // free emails given on first start
	TimeoutSeconds int    `json:"timeout_seconds"` // HTTP timeout
	AdminID        int64  `json:"admin_id"`        // admin telegram ID
	DBPath         string `json:"db_path"`         // path to the database
	Debug          bool   `json:"debug"`           // debug mode
	StatPassword   string `json:"stat_password"`   // password for statistics
	Certificate    string `json:"certificate"`     // certificate path for STARTTLS
	CertificateKey string `json:"certificate_key"` // certificate key path for STARTTLS
}

func readConfig(path string) *config {
	file, err := os.Open(filepath.Clean(path))
	checkErr(err)
	defer func() { checkErr(file.Close()) }()
	return parseConfig(file)
}

func parseConfig(r io.Reader) *config {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	cfg := &config{}
	err := decoder.Decode(cfg)
	checkErr(err)
	checkErr(checkConfig(cfg))
	return cfg
}

func checkConfig(cfg *config) error {
	if cfg.ListenAddress == "" {
		return errors.New("configure listen_address")
	}
	if cfg.ListenPath == "" {
		return errors.New("configure listen_path")
	}
	if cfg.BotToken == "" {
		return errors.New("configure bot_token")
	}
	if cfg.TimeoutSeconds == 0 {
		return errors.New("configure timeout_seconds")
	}
	if cfg.AdminID == 0 {
		return errors.New("configure admin_id")
	}
	if cfg.DBPath == "" {
		return errors.New("configure db_path")
	}
	if cfg.StatPassword == "" {
		return errors.New("configure stat_password")
	}
	if cfg.FreeEmails == 0 {
		return errors.New("configure free_emails")
	}
	if cfg.Certificate == "" {
		return errors.New("configure certificate")
	}
	if cfg.CertificateKey == "" {
		return errors.New("configure certificate_key")
	}
	return nil
}

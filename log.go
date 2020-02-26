package main

import "log"

// lerr logs an error
func lerr(format string, v ...interface{}) { log.Printf("[ERROR] "+format, v...) }

// linf logs an info message
func linf(format string, v ...interface{}) { log.Printf("[INFO] "+format, v...) }

// ldbg logs a debug message
func ldbg(format string, v ...interface{}) { log.Printf("[DEBUG] "+format, v...) }

// lsmtpd logs a debug message in smtpd library
func lsmtpd(format string, v ...interface{}) { log.Printf("[SMTPD] "+format, v...) }

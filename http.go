package main

import (
	"net/http"
	"time"
)

// HTTPClientWithTimeoutAndAddress returns HTTP client bound to specific IP address
func HTTPClientWithTimeoutAndAddress(timeoutSeconds int) *http.Client {
	client := &http.Client{
		Timeout: time.Second * time.Duration(timeoutSeconds),
	}
	return client
}
